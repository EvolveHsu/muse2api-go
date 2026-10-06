// Package server 对应 app.py：路由、鉴权、CORS、FastAPI 的两套错误壳与响应写法。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"muse2api-go/internal/config"
	"muse2api-go/internal/engine"
	"muse2api-go/internal/store"
)

// AppError 对应 HTTPException(status_code, detail, headers)。
type AppError struct {
	Status  int
	Detail  any
	Headers map[string]string
}

func (e *AppError) Error() string {
	s, ok := e.Detail.(string)
	if !ok {
		return http.StatusText(e.Status)
	}
	return s
}

func httpError(status int, detail any) *AppError {
	return &AppError{Status: status, Detail: detail}
}

// errType 对应 _err_type()。
func errType(status int) string {
	switch {
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	}
	return "invalid_request_error"
}

type reqCtx struct {
	W    http.ResponseWriter
	R    *http.Request
	Args map[string]string
}

type handlerFunc func(c *reqCtx) (any, error)

type segment struct {
	lit   string
	param bool
}

type route struct {
	method string
	segs   []segment
	auth   bool
	h      handlerFunc
	path   string
}

// App 持有 app.py 的模块级状态：store/engine 单例、两把锁、路由表。
type App struct {
	CFG    *config.Config
	Store  *store.Store
	Engine *engine.Engine
	// browser 对应 GEN_LOCK（全局串行），imgTaskLock 对应 IMAGE_TASK_LOCK（只管入队那一段）。
	browser     *browserLock
	imgTaskLock *sync.Mutex

	routes []*route

	// mu 护着运行时可改的配置（目前只有 API Key 轮换）；
	// updMu/kaMu 对应 _UPDATE_CACHE 与保活状态那两份模块级可变对象。
	mu       sync.Mutex
	updMu    sync.Mutex
	updCache updateCache
	kaMu     sync.Mutex
	kaState  keepaliveState

	// now 是墙钟（落盘时间戳），mono 是单调钟（deadline）。Python 也是这么混用的，
	// 不「顺手统一」：前者会被系统校时拉动，后者不会 —— Go 的 time.Time 自带单调读数，
	// 只要两端都取自 time.Now，Sub/Before 走的就是单调钟。
	now  func() time.Time
	mono func() time.Time
}

func NewApp(cfg *config.Config, st *store.Store, eng *engine.Engine) *App {
	a := &App{
		CFG: cfg, Store: st, Engine: eng,
		browser:     newBrowserLock(),
		imgTaskLock: &sync.Mutex{},
		now:         time.Now,
		mono:        time.Now,
	}
	a.registerRoutes()
	return a
}

// --- 路由表 ---

// pat 支持 {name} 形式的单段通配，语义与 Starlette 的 [^/]+ 一致：
// 一个参数只吃一段，段里出现（解码后的）'/' 就算整条路径不匹配 → 404，
// 这跟 Python 对 /v1/media/..%2Faccounts.json 的行为逐字对齐。
func parsePattern(p string) []segment {
	var out []segment
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			out = append(out, segment{lit: s[1 : len(s)-1], param: true})
			continue
		}
		out = append(out, segment{lit: s})
	}
	return out
}

func (a *App) add(method, pattern string, auth bool, h handlerFunc) {
	a.routes = append(a.routes, &route{method: method, path: pattern,
		segs: parsePattern(pattern), auth: auth, h: h})
}

func matchPath(segs []segment, path string) (map[string]string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != len(segs) {
		return nil, false
	}
	args := map[string]string{}
	for i, s := range segs {
		if s.param {
			if parts[i] == "" {
				return nil, false
			}
			args[s.lit] = parts[i]
			continue
		}
		if parts[i] != s.lit {
			return nil, false
		}
	}
	return args, true
}

// ServeHTTP 复刻 FastAPI 的分派顺序：先 CORS，再路由匹配（404/405 由路由器给出，
// 走的是 Starlette 默认处理器 —— 未匹配的 /v1 路径**不会**套上 OpenAI 错误壳，
// 金样本 media_enc_slash_only / v1_unmatched 就是把这条钉住），最后才轮到鉴权依赖。
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	applyCORS(w, r)
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		preflight(w, r)
		return
	}
	var methodMismatch bool
	for _, rt := range a.routes {
		args, ok := matchPath(rt.segs, r.URL.Path)
		if !ok {
			continue
		}
		if strings.ToUpper(r.Method) != rt.method {
			methodMismatch = true
			continue
		}
		a.dispatch(w, r, rt, args)
		return
	}
	if methodMismatch {
		a.writeRouterError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	a.writeRouterError(w, http.StatusNotFound, "Not Found")
}

// writeRouterError：路由自身的 404/405 出自 Starlette 的默认异常处理，
// 它**不看路径前缀** —— 即便请求的是 /v1/xxx 这种没注册上的地址，回的还是 {"detail":...}
// 而不是 OpenAI 错误壳。金样本 media_enc_slash_only / v1_unmatched 就是把这条钉住的
// （handler 里主动 raise 的 404 才会套壳，见 media_missing）。
func (a *App) writeRouterError(w http.ResponseWriter, status int, detail string) {
	jsonOut(w, status, map[string]any{"detail": detail})
}

func (a *App) dispatch(w http.ResponseWriter, r *http.Request, rt *route, args map[string]string) {
	ctx := &reqCtx{W: w, R: r, Args: args}
	if rt.auth {
		if err := a.auth(r); err != nil {
			ae := err.(*AppError)
			a.writeError(w, r, ae.Status, ae.Detail, ae.Headers)
			return
		}
	}
	res, err := rt.h(ctx)
	if err != nil {
		if ae, ok := err.(*AppError); ok {
			a.writeError(w, r, ae.Status, ae.Detail, ae.Headers)
			return
		}
		// 未预期异常：Starlette 的 ServerErrorMiddleware 只回 500 "Internal Server Error"，
		// 金样本 edits_badjson 钉住了它。Go 这边同样不把内部错误文案漏给客户端。
		writePlain(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	switch v := res.(type) {
	case nil:
		jsonOut(w, http.StatusOK, map[string]any{})
	case *rawResponse:
		v.write(ctx)
	default:
		jsonOut(w, http.StatusOK, v)
	}
}

// --- 鉴权（app.py: auth()） ---

func (a *App) auth(r *http.Request) error {
	if a.CFG.APIKey == "" {
		return nil
	}
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return httpError(401, "缺少 Authorization: Bearer <key>")
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	if token == "" || token != a.CFG.APIKey {
		return httpError(401, "API key 无效")
	}
	return nil
}

// --- 响应写法（对齐 FastAPI 的 JSONResponse / PlainTextResponse） ---

// jsonOut：FastAPI 用 json.dumps(..., ensure_ascii=False, separators=(",", ":"))，
// 即紧凑分隔符、非 ASCII 原样输出、**不**转义 < > &，且结尾没有换行。
func jsonOut(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		writePlain(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeError 对应 app.py 的 _http_exc：/v1/* 换成 OpenAI 的错误壳，其余保持 {"detail":...}，
// 因为管理面板是按 detail 取信息的。
func (a *App) writeError(w http.ResponseWriter, r *http.Request, status int, detail any, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		msg, ok := detail.(string)
		if !ok {
			b, _ := json.Marshal(detail)
			msg = string(b)
		}
		jsonOut(w, status, map[string]any{"error": map[string]any{
			"message": msg, "type": errType(status), "param": nil, "code": status}})
		return
	}
	jsonOut(w, status, map[string]any{"detail": detail})
}

// --- CORS（app.py 的 add_middleware：allow_origins=["*"] 等） ---

const corsMethods = "GET, POST, PATCH, DELETE, OPTIONS"

func applyCORS(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("vary", "Origin")
	if r.Header.Get("Origin") == "" {
		return
	}
	h.Set("access-control-allow-origin", "*")
	h.Set("access-control-expose-headers", "*")
}

func preflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if req := r.Header.Get("Access-Control-Request-Method"); req != "" {
		h.Set("access-control-allow-methods", corsMethods)
	}
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		h.Set("access-control-allow-headers", req)
	}
	// Starlette 在这条路上会把四个请求头名一起写进单个 Vary，金样本逐字记着。
	h.Set("vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers, Access-Control-Request-Private-Network")
	h.Set("access-control-allow-origin", "*")
	h.Set("access-control-max-age", "600")
	writePlain(w, http.StatusOK, "OK")
}
