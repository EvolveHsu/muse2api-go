package engine

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// extractBytes 对应 extract_bytes()：在页面里 fetch 成 base64 取回字节，失败重试 4 次。
func (e *Engine) extractBytes(src string, expect string, retries int) ([]byte, string, string, error) {
	if retries <= 0 {
		retries = 4
	}
	last := "未知"
	expr, err := pyFormat(extractJS, jsonString(src), jsonString(expect))
	if err != nil {
		return nil, "", "", &ErrGeneration{Msg: err.Error()}
	}
	for i := 0; i < retries; i++ {
		raw, err := e.page.JS(expr, true, 600*time.Second)
		if err != nil {
			return nil, "", "", err
		}
		info := map[string]any{}
		if raw.Kind == KString {
			if err := json.Unmarshal([]byte(raw.Str), &info); err != nil {
				info = map[string]any{"ok": false,
					"err": "解析失败 " + clipStr(raw.Str, 150)}
			}
		} else if raw.Kind == KObject {
			for k, v := range raw.Obj {
				info[k] = v.Str2()
			}
			if okv, ok := raw.Obj["ok"]; ok {
				info["ok"] = okv.Kind != KNull && okv.Truthy()
			}
		} else {
			// DEVIATION: CDP 返回既不是字符串也不是对象（比如 undefined→null）时，
			// Python 在 info.get("ok") 上直接 AttributeError → 未捕获 → 对外 500；
			// Go 把它当一次「解析失败」继续重试。只在页面 JS 抛了非预期类型的异常时才走得到。
			info = map[string]any{"ok": false, "err": "解析失败 " + clipStr(raw.Str2(), 150)}
		}
		if ok, _ := info["ok"].(bool); ok {
			b64, _ := info["b64"].(string)
			data, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				return nil, "", "", &ErrGeneration{Msg: "未能取回生成结果: base64 解码失败"}
			}
			mimeStr, _ := info["mime"].(string)
			urlStr, _ := info["url"].(string)
			return data, mimeStr, urlStr, nil
		}
		if v, ok := info["err"]; ok {
			last = fmt.Sprint(v)
		}
		e.sleep(2 * time.Second)
	}
	return nil, "", "", &ErrGeneration{Msg: "未能取回生成结果: " + last}
}

func clipStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// downloadFallback 对应 _download_fallback()：只点"这个结果自己的"下载按钮，
// 找不到就返回 nil —— 绝不点全局按钮，否则会把上传的原图当结果拿回来。
func (e *Engine) downloadFallback(src string, timeout int) (string, error) {
	dir := e.CFG.DownloadDir()
	// Python 这里没有 try：下载目录不存在就直接抛，Go 也不能吞掉当作空目录。
	before, err := listNames(dir)
	if err != nil {
		return "", err
	}
	expr, err := pyFormat(jsDownloadFallbackL965, jsonString(src))
	if err != nil {
		return "", &ErrGeneration{Msg: err.Error()}
	}
	clicked, err := e.page.JS(expr, false, 0)
	if err != nil {
		return "", err
	}
	if clicked.Str2() == "none" {
		return "", nil
	}
	if timeout <= 0 {
		timeout = 180
	}
	deadline := e.now().Add(time.Duration(timeout) * time.Second)
	for e.now().Before(deadline) {
		e.sleep(1500 * time.Millisecond)
		now, err := listNames(dir)
		if err != nil {
			return "", err
		}
		var fresh []string
		for f := range now {
			if !before[f] && !strings.HasSuffix(f, ".crdownload") {
				fresh = append(fresh, f)
			}
		}
		if len(fresh) > 0 {
			best := fresh[0]
			bestMT := modTime(filepath.Join(dir, best))
			for _, f := range fresh[1:] {
				if mt := modTime(filepath.Join(dir, f)); mt.After(bestMT) {
					best, bestMT = f, mt
				}
			}
			p := filepath.Join(dir, best)
			if st, err := os.Stat(p); err == nil && st.Size() > 0 {
				return p, nil
			}
		}
	}
	return "", nil
}

func listNames(dir string) (map[string]bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ents))
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out, nil
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// fetchURL 是参考图下载注入点（测试可换成假实现）。
type fetchURL func(url string) ([]byte, string, error)

var liveFetch fetchURL = func(u string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "image/png"
	}
	return data, ct, nil
}

// normalizeImage 对应 _normalize_image()：data:/http(s)/本地文件/裸 base64 归一成 (b64, mime)。
func (e *Engine) normalizeImage(img string) (string, string) {
	if img == "" {
		return "", "image/png"
	}
	img = strings.TrimSpace(img)
	if strings.HasPrefix(img, "data:") {
		head, tail, has := strings.Cut(img[5:], ",")
		mimeType := "image/png"
		if i := strings.Index(head, ";"); i >= 0 {
			mimeType = strings.TrimSpace(head[:i])
		}
		if !has {
			return "", mimeType
		}
		return strings.TrimSpace(tail), mimeType
	}
	if strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://") {
		f := e.Fetch
		if f == nil {
			f = liveFetch
		}
		data, ct, err := f(img)
		if err != nil {
			log.Printf("下载远程参考图失败: %v", err)
			return "", "image/png"
		}
		return base64.StdEncoding.EncodeToString(data), ct
	}
	if st, err := os.Stat(img); err == nil && !st.IsDir() {
		data, err := os.ReadFile(img)
		if err != nil {
			log.Printf("读取本地参考图失败: %v", err)
			return "", "image/png"
		}
		mimeType := mime.TypeByExtension(filepath.Ext(img))
		if mimeType == "" {
			mimeType = "image/png"
		}
		return base64.StdEncoding.EncodeToString(data), mimeType
	}
	return img, "image/png"
}

// clearAttachments 对应 _clear_attachments()（兼容中/英文 UI）。
func (e *Engine) clearAttachments() {
	if _, err := e.page.JS(jsClearAttachmentsL1027, false, 0); err != nil {
		return
	}
	e.sleep(300 * time.Millisecond)
}

// attachImage 对应 _attach_image()：用 DataTransfer 把参考图塞进 composer 的 file input。
func (e *Engine) attachImage(imageData string) error {
	if imageData == "" {
		return nil
	}
	b64, mimeType := e.normalizeImage(imageData)
	if b64 == "" {
		return &ErrGeneration{Msg: "参考图读取失败，已停止生成"}
	}
	e.clearAttachments()

	expr, err := pyFormat(injectJS, jsonString(b64), jsonString(mimeType))
	if err != nil {
		return &ErrGeneration{Msg: "附加参考图失败，已停止生成"}
	}
	raw, err := e.page.JS(expr, false, 0)
	if err != nil {
		return &ErrGeneration{Msg: "附加参考图失败，已停止生成"}
	}
	ok := false
	switch raw.Kind {
	case KString:
		var m map[string]any
		if json.Unmarshal([]byte(raw.Str), &m) == nil {
			ok, _ = m["ok"].(bool)
		}
	case KObject:
		if v, has := raw.Obj["ok"]; has {
			ok = v.Truthy()
		}
	}
	if !ok {
		return &ErrGeneration{Msg: "附加参考图失败，已停止生成"}
	}

	deadline := e.now().Add(15 * time.Second)
	gotConfirm := false
	for e.now().Before(deadline) {
		v, err := e.page.JS(jsAttachImageL1105, false, 0)
		if err != nil {
			return &ErrGeneration{Msg: "参考图上传未确认，已停止生成"}
		}
		if v.Truthy() {
			gotConfirm = true
			break
		}
		e.sleep(300 * time.Millisecond)
	}
	if !gotConfirm {
		return &ErrGeneration{Msg: "参考图上传未确认，已停止生成"}
	}
	e.sleep(500 * time.Millisecond)
	return nil
}

// pickExt 对应 _pick_ext()。
func pickExt(mimeType, url, expect string) string {
	m := strings.ToLower(mimeType)
	for _, kv := range [][2]string{{"mp4", ".mp4"}, {"webm", ".webm"}, {"png", ".png"},
		{"jpeg", ".jpg"}, {"jpg", ".jpg"}, {"webp", ".webp"}, {"gif", ".gif"}} {
		if strings.Contains(m, kv[0]) {
			return kv[1]
		}
	}
	lu := strings.ToLower(url)
	for _, x := range []string{".mp4", ".webm", ".png", ".jpg", ".webp"} {
		if strings.Contains(lu, x) {
			return x
		}
	}
	if expect == "video" {
		return ".mp4"
	}
	return ".png"
}

// GenResult 对应 generate() 的返回 dict。
type GenResult struct {
	Path       string `json:"path"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	Ext        string `json:"ext"`
	MimeType   string `json:"mime"`
	Kind       string `json:"kind"`
	Via        string `json:"via"`
	Attachment string `json:"attachment"`
	W          int    `json:"w"`
	H          int    `json:"h"`
}

// Generate 对应 generate()：重置会话 → 挂参考图 → 记基线 → 发送 → 等新附件 → 取字节 → 落盘。
func (e *Engine) Generate(cookies map[string]string, prompt, expect string, timeout int,
	expires map[string]int, accountID string, onProgress func(int),
	referenceImage string, stop <-chan struct{}) (*GenResult, error) {

	// Python 的 240 是形参默认值，调用方一律显式传 timeout（路由层已经用 `or` 兜过底），
	// 所以这里不能再自作主张把 0 或负数改成 240 —— 负数在 Python 那边就是「立刻超时」。
	if _, err := e.ensurePage(cookies, expires, accountID); err != nil {
		return nil, err
	}
	e.ResetThread(false)
	e.scrollBottom()
	if referenceImage != "" {
		if err := e.attachImage(referenceImage); err != nil {
			return nil, err
		}
	} else {
		e.clearAttachments()
	}

	attsBefore := e.Attachments()
	baselineSources := map[string]bool{}
	for _, a := range attsBefore {
		for _, v := range []string{a.Src, a.VSrc, a.ISrc} {
			if v != "" {
				baselineSources[v] = true
			}
		}
	}
	baselineSrc := ""
	if len(attsBefore) > 0 {
		baselineSrc = attsBefore[len(attsBefore)-1].Src
	}
	baseAgentCnt := e.agentCount()
	sent, err := e.send(prompt)
	if err != nil {
		return nil, err
	}
	if sent != "clicked" && sent != "enter-sent" {
		return nil, &ErrGeneration{Msg: "提示词发送未确认，已停止生成"}
	}
	att, found, err := e.WaitAttachment(baselineSrc, timeout, expect, onProgress,
		baseAgentCnt, len(attsBefore), stop, baselineSources)
	if err != nil {
		return nil, err
	}
	if !found {
		e.debugDump("no-attachment")
		return nil, &ErrGeneration{Msg: "等待生成超时，未出现新的生成结果"}
	}

	if err := os.MkdirAll(e.CFG.MediaDir(), 0o755); err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	selectedSrc := att.Src
	if expect == "video" && att.VSrc != "" {
		selectedSrc = att.VSrc
	}
	var data []byte
	var mimeType, url string
	if b, m, u, err := e.extractBytes(selectedSrc, expect, 4); err == nil {
		data, mimeType, url = b, m, u
	} else {
		e.debugDump("extract-fail")
	}

	if len(data) > 0 {
		ext := pickExt(mimeType, url, expect)
		name := uuid4Hex() + ext
		dst := filepath.Join(e.CFG.MediaDir(), name)
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return nil, &ErrGeneration{Msg: err.Error()}
		}
		return &GenResult{Path: dst, Filename: name, Size: int64(len(data)), Ext: ext,
			MimeType: mimeType, Kind: kindOfExt(ext), Via: "blob",
			Attachment: att.Tid, W: att.W, H: att.H}, nil
	}

	path, err := e.downloadFallback(selectedSrc, 180)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, &ErrGeneration{Msg: "已生成但未能取回文件"}
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		ext = ".bin"
	}
	name := uuid4Hex() + ext
	dst := filepath.Join(e.CFG.MediaDir(), name)
	if err := os.Rename(path, dst); err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	st, _ := os.Stat(dst)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	return &GenResult{Path: dst, Filename: name, Size: size, Ext: ext,
		MimeType: "", Kind: kindOfExt(ext), Via: "download",
		Attachment: att.Tid, W: att.W, H: att.H}, nil
}

func kindOfExt(ext string) string {
	switch ext {
	case ".mp4", ".webm", ".mov":
		return "video"
	}
	return "image"
}

func uuid4Hex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

// Quota 对应 quota()：打开 Settings 面板读额度。返回结构化结果；读不到时 raise。
func (e *Engine) Quota(cookies map[string]string, expires map[string]int) (map[string]any, error) {
	if _, err := e.ensurePage(cookies, expires, ""); err != nil {
		return nil, err
	}
	p := e.page
	e.sleep(time.Second)

	clickExpr, err := pyFormat(clickJS, jsonString(`button[aria-label="Settings"]`))
	if err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	raw, err := p.JS(clickExpr, false, 0)
	if err != nil {
		return nil, err
	}
	if !raw.Truthy() {
		return nil, &ErrGeneration{Msg: "找不到 Settings 按钮"}
	}
	pt, err := readPoint(raw.Str2())
	if err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	e.clickPoint(pt[0], pt[1])
	e.sleep(1600 * time.Millisecond)

	raw, err = p.JS(jsQuotaL438, false, 0)
	if err != nil {
		return nil, err
	}
	if !raw.Truthy() {
		return nil, &ErrGeneration{Msg: "Settings 菜单未弹出"}
	}
	pt, err = readPoint(raw.Str2())
	if err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	e.clickPoint(pt[0], pt[1])
	e.sleep(3 * time.Second)

	txt := ""
	for i := 0; i < 6; i++ {
		v, err := p.JS(jsQuotaL458, false, 0)
		if err != nil {
			return nil, err
		}
		txt = v.Str2()
		if strings.Contains(txt, "Usage") || strings.Contains(txt, "used") {
			break
		}
		e.sleep(1200 * time.Millisecond)
	}

	for _, t := range [2]string{"keyDown", "keyUp"} {
		if _, err := p.Send("Input.dispatchKeyEvent", map[string]any{
			"type": t, "key": "Escape", "code": "Escape",
			"windowsVirtualKeyCode": 27, "nativeVirtualKeyCode": 27}, 0); err != nil {
			return nil, err
		}
	}
	e.sleep(500 * time.Millisecond)
	return parseQuota(txt), nil
}

func readPoint(s string) ([2]int, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return [2]int{}, err
	}
	x, _ := toIntLike(m["x"])
	y, _ := toIntLike(m["y"])
	return [2]int{x, y}, nil
}

func toIntLike(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}

var (
	planRe      = regexp.MustCompile(`(?i)^(.+?)\s*plan$`)
	weeklyRe    = regexp.MustCompile(`Weekly limit resets? on (.+)`)
	pctUsedRe   = regexp.MustCompile(`(\d+)%\s*used`)
	pctUsedPair = regexp.MustCompile(`(\d+)%\s*used\s*\(([^)]+)\)`)
)

// parseQuota 对应 _parse_quota()。
func parseQuota(txt string) map[string]any {
	var lines []string
	for _, ln := range strings.Split(txt, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	tail := lines
	if len(tail) > 40 {
		tail = tail[:40]
	}
	out := map[string]any{"raw": strings.Join(tail, "\n")}
	for _, ln := range lines {
		if planRe.MatchString(ln) {
			out["plan"] = ln
			break
		}
	}
	if m := weeklyRe.FindStringSubmatch(txt); m != nil {
		out["weekly_reset"] = strings.TrimSpace(m[1])
	}
	if m := pctUsedRe.FindStringSubmatch(txt); m != nil {
		n, _ := strconv.Atoi(m[1])
		out["weekly_used_pct"] = n
	}
	if m := pctUsedPair.FindStringSubmatch(txt); m != nil {
		n, _ := strconv.Atoi(m[1])
		out["extra_used_pct"] = n
		out["extra_left"] = strings.TrimSpace(m[2])
	}
	if strings.Contains(txt, "Never expires") {
		out["extra_expires"] = "never"
	}
	_, hasPct := out["weekly_used_pct"]
	planVal, hasPlan := out["plan"]
	out["found"] = hasPlan && planVal != "" || hasPct
	return out
}

func sortStrings(s []string) { sort.Strings(s) }
