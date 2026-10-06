package server

import (
	"crypto/md5" //nolint:gosec // etag 用 MD5 是 Starlette FileResponse 的算法，不是安全用途
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"muse2api-go/internal/pyval"
)

// MODELS 对应 app.py 的 MODELS 常量；字段顺序即 Python dict 的插入顺序。
var MODELS = []modelEntry{
	{ID: "muse-spark", Object: "model", OwnedBy: "muse",
		Description: "Muse Spark —— 文本 / 代码对话（网页免费额度，支持流式）"},
	{ID: "muse-image", Object: "model", OwnedBy: "muse",
		Description: "Muse Image —— 文生图 / 图像编辑（网页免费额度）"},
	{ID: "muse-video", Object: "model", OwnedBy: "muse",
		Description: "Muse Video —— 文生视频 / 图生视频（网页免费额度）"},
}

type modelEntry struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	OwnedBy     string `json:"owned_by"`
	Description string `json:"description"`
}

// rawResponse 承载非 JSON 的响应体（文件、HTML、SSE）。
type rawResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
	// Stream 在写头之后接管 body 的写出（SSE 与文件都用它）。
	Stream func(c *reqCtx) error
}

func (r *rawResponse) write(c *reqCtx) {
	for k, v := range r.Headers {
		c.W.Header().Set(k, v)
	}
	if r.Stream != nil {
		if r.Status != 0 {
			c.W.WriteHeader(r.Status)
		}
		if err := r.Stream(c); err != nil {
			return
		}
		return
	}
	st := r.Status
	if st == 0 {
		st = http.StatusOK
	}
	c.W.WriteHeader(st)
	_, _ = c.W.Write(r.Body)
}

// mimeByExt 抄的是**容器里那份 Python 的 mimetypes 判定结果**（探针取回），
// 不是 Go 的 mime 表 —— 两边对同一个扩展名会给不同结论：
// 容器里 .webp 猜不出来，于是 /v1/media/x.webp 的 content-type 是 application/octet-stream，
// 而 Go 的表会给出 image/webp。金样本 media_real_* 就是把这条钉住的。
var mimeByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".mp4": "video/mp4", ".webm": "video/webm",
	".mov": "video/quicktime", ".txt": "text/plain", ".json": "application/json",
	".html": "text/html", ".js": "application/javascript", ".py": "text/x-python",
	".zip": "application/zip", ".bin": "application/octet-stream",
	".heic": "image/heic",
	// .webp 故意留空：Python 侧猜不出来，落到 octet-stream。
}

func guessType(name string) string {
	return mimeByExt[strings.ToLower(filepath.Ext(name))]
}

// starletteEtag 复刻 FileResponse 的算法：etag_base = str(st_mtime) + "-" + str(st_size)，
// etag = '"' + md5(etag_base).hexdigest() + '"'。
// str(float) 必须由 pyval.Float 给出 —— Go 的默认写法会把 1791212303.8032646 变成
// "1.7912123038032646e+09"，同一个文件的 etag 就跟 Python 完全不同，客户端的条件请求会全部退化。
func starletteEtag(mtime time.Time, size int64) string {
	// 与 Python 一样：st_mtime 是 float(tv_sec + tv_nsec/1e9)，保留小数部分。
	f := float64(mtime.Unix()) + float64(mtime.Nanosecond())/1e9
	base := pyval.Float(f) + "-" + strconv.FormatInt(size, 10)
	sum := md5.Sum([]byte(base)) //nolint:gosec
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// fileResponse 复刻 starlette 1.7.0 FileResponse 的头集合。
func fileResponse(c *reqCtx, path, filename string, st os.FileInfo) *rawResponse {
	mt := guessType(filename)
	if mt == "" {
		mt = "application/octet-stream"
	}
	if strings.HasPrefix(mt, "text/") {
		mt += "; charset=utf-8"
	}
	mtime := st.ModTime()
	return &rawResponse{
		Headers: map[string]string{
			"content-type":  mt,
			"accept-ranges": "bytes",
			"last-modified": mtime.UTC().Format(http.TimeFormat),
			"etag":          starletteEtag(mtime, st.Size()),
		},
		Stream: func(cc *reqCtx) error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			// ServeContent 复用上面已设好的 content-type / last-modified / etag，
			// 并接管 Range、HEAD、304 —— 与 Starlette 的 FileResponse 行为对应。
			// Starlette 在 206 时会把 etag 原文复制进 content-range 的实体标签位，
			// 而 Go 只会替换成自己生成的弱标记，所以用 etagGuard 兜住那个位置。
			origTag := cc.W.Header().Get("etag")
			http.ServeContent(etagGuard{cc.W, origTag}, cc.R, filename, mtime, f)
			return nil
		},
	}
}

// etagGuard 只拦一件事：ServeContent 生成 Content-Range 时塞的是它自己那套 Go 风格弱标记
// （`W/"inode-mtime-size"`），而 Starlette 是把响应头里的 etag 原样复制进那个位置。
type etagGuard struct {
	http.ResponseWriter
	tag string
}

func (g etagGuard) Header() http.Header { return g.ResponseWriter.Header() }

func (g etagGuard) Write(b []byte) (int, error) {
	h := g.ResponseWriter.Header()
	if g.tag != "" {
		if cr := h.Get("Content-Range"); strings.HasPrefix(cr, "bytes ") {
			// Go 在 cr 末尾拼的是它自己的 W/"..."；换成响应头里那个 etag 原文。
			if i := strings.LastIndexByte(cr, ' '); i > 5 {
				h.Set("Content-Range", cr[:i]+" "+g.tag)
			}
		}
	}
	return g.ResponseWriter.Write(b)
}

// registerRoutes 先挂上这一批只读路由；生成类路由（要抢浏览器锁的那些）在后面的提交里补。
func (a *App) registerRoutes() {
	a.add("GET", "/healthz", false, func(c *reqCtx) (any, error) {
		return map[string]any{"status": "ok", "time": int(a.now().Unix())}, nil
	})
	a.add("GET", "/readyz", false, func(c *reqCtx) (any, error) {
		enabled := 0
		for _, acc := range a.Store.ListAccounts() {
			if v, ok := acc["enabled"].(bool); !ok || v {
				enabled++
			}
		}
		st := "no_account"
		if enabled > 0 {
			st = "ready"
		}
		return map[string]any{"status": st, "accounts": enabled,
			"browser_running": a.Engine != nil && a.Engine.BrowserProcUp()}, nil
	})
	a.add("GET", "/v1/models", true, func(c *reqCtx) (any, error) {
		return map[string]any{"object": "list", "data": MODELS}, nil
	})
	a.add("GET", "/v1/media/{name}", false, func(c *reqCtx) (any, error) {
		name := c.Args["name"]
		if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			return nil, httpError(400, "非法文件名")
		}
		p := filepath.Join(a.CFG.MediaDir(), name)
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return nil, httpError(404, "文件不存在")
		}
		return fileResponse(c, p, name, st), nil
	})
	a.registerV1()
	a.registerAdmin()
	a.registerDocs()
}

var _ = time.Second
