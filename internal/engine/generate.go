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
	"strconv"
	"strings"
	"time"

	uninorm "golang.org/x/text/unicode/norm"
)

// extractBytes 对应 extract_bytes()：在页面里 fetch 成 base64 取回字节，失败重试 4 次。
func (e *Engine) extractBytes(page Session, src string, expect string, retries int) ([]byte, string, string, error) {
	if retries <= 0 {
		retries = 4
	}
	last := "未知"
	expr, err := pyFormat(extractJS, jsonString(src), jsonString(expect))
	if err != nil {
		return nil, "", "", &ErrGeneration{Msg: err.Error()}
	}
	for i := 0; i < retries; i++ {
		raw, err := page.JS(expr, true, 600*time.Second)
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
func (e *Engine) downloadFallback(page Session, src string, timeout int) (string, error) {
	dir := e.CFG.DownloadDir()
	// Python 这里没有 try：下载目录不存在就直接抛，Go 也不能吞掉当作空目录。
	before, err := listNames(dir)
	if err != nil {
		return "", err
	}
	expr, err := pyFormat(jsDownloadButton, jsonString(src))
	if err != nil {
		return "", &ErrGeneration{Msg: err.Error()}
	}
	clicked, err := page.JS(expr, false, 0)
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
func (e *Engine) clearAttachments(page Session) {
	if _, err := page.JS(jsClearAttachments, false, 0); err != nil {
		return
	}
	e.sleep(300 * time.Millisecond)
}

// attachImage 对应 _attach_image()：用 DataTransfer 把参考图塞进 composer 的 file input。
func (e *Engine) attachImage(page Session, imageData string) error {
	if imageData == "" {
		return nil
	}
	b64, mimeType := e.normalizeImage(imageData)
	if b64 == "" {
		return &ErrGeneration{Msg: "参考图读取失败，已停止生成"}
	}
	e.clearAttachments(page)

	expr, err := pyFormat(injectJS, jsonString(b64), jsonString(mimeType))
	if err != nil {
		return &ErrGeneration{Msg: "附加参考图失败，已停止生成"}
	}
	raw, err := page.JS(expr, false, 0)
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
		v, err := page.JS(jsAttachConfirm, false, 0)
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
//
// 上游 1.6.x 的两处行为变化：
//   - 截止线从 generate() 入口开始算，准备阶段吃掉的秒数会从等待里扣掉；
//   - 视频任务拒绝以图片冒充成品：提取/下载得到的 kind 不是 video 就报错。
func (e *Engine) Generate(ctx *PageCtx, cookies map[string]string, prompt, expect string, timeout int,
	expires map[string]int, accountID string, onProgress func(int),
	referenceImage string, stop <-chan struct{}) (*GenResult, error) {

	deadline := e.now().Add(time.Duration(timeout) * time.Second)

	// Python 的 240 是形参默认值，调用方一律显式传 timeout（路由层已经用 `or` 兜过底），
	// 所以这里不能再自作主张把 0 或负数改成 240 —— 负数在 Python 那边就是「立刻超时」。
	if _, err := e.ensurePage(ctx, cookies, expires, accountID); err != nil {
		return nil, err
	}
	e.ResetThread(ctx, false)
	page := ctx.Page
	e.scrollBottom(page)
	if referenceImage != "" {
		if err := e.attachImage(page, referenceImage); err != nil {
			return nil, err
		}
	} else {
		e.clearAttachments(page)
	}

	attsBefore := e.Attachments(page)
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
	baseAgentCnt := e.agentCount(page)
	if !e.now().Before(deadline) {
		return nil, &ErrGeneration{Msg: "任务准备超时，已停止发送"}
	}
	sent, err := e.send(page, prompt)
	if err != nil {
		return nil, err
	}
	if sent != "clicked" && sent != "enter-sent" {
		return nil, &ErrGeneration{Msg: "提示词发送未确认，已停止生成"}
	}
	remaining := deadline.Sub(e.now()).Seconds()
	if remaining < 0 {
		remaining = 0
	}
	att, found, err := e.WaitAttachment(page, baselineSrc, remaining, expect, onProgress,
		baseAgentCnt, len(attsBefore), stop, baselineSources)
	if err != nil {
		return nil, err
	}
	if !found {
		e.debugDump(page, "no-attachment")
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
	if b, m, u, err := e.extractBytes(page, selectedSrc, expect, 4); err == nil {
		data, mimeType, url = b, m, u
	} else {
		e.debugDump(page, "extract-fail")
	}

	if len(data) > 0 {
		ext := pickExt(mimeType, url, expect)
		kind := kindOfExt(ext)
		if expect == "video" && kind != "video" {
			return nil, &ErrGeneration{Msg: fmt.Sprintf(
				"期望生成视频，但提取成品类型为图片 (%s)，拒绝以图片冒充视频交付", ext)}
		}
		name := uuid4Hex() + ext
		dst := filepath.Join(e.CFG.MediaDir(), name)
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return nil, &ErrGeneration{Msg: err.Error()}
		}
		return &GenResult{Path: dst, Filename: name, Size: int64(len(data)), Ext: ext,
			MimeType: mimeType, Kind: kind, Via: "blob",
			Attachment: att.Tid, W: att.W, H: att.H}, nil
	}

	path, err := e.downloadFallback(page, selectedSrc, 180)
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
	kind := kindOfExt(ext)
	if expect == "video" && kind != "video" {
		return nil, &ErrGeneration{Msg: fmt.Sprintf(
			"期望生成视频，但下载成品类型为图片 (%s)，拒绝以图片冒充视频交付", ext)}
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
		MimeType: "", Kind: kind, Via: "download",
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

// Quota 对应 quota()：打开 Settings 面板读额度。读不到有效用量时报错，
// 由上层决定是否沿用旧缓存（对应 app.py 的 except 分支）。
func (e *Engine) Quota(ctx *PageCtx, cookies map[string]string, expires map[string]int,
	accountID string) (map[string]any, error) {
	if _, err := e.ensurePage(ctx, cookies, expires, accountID); err != nil {
		return nil, err
	}
	p := ctx.Page
	e.dismissQuotaUI(p)
	defer func() {
		if err := recover(); err != nil {
			log.Printf("关闭额度设置面板失败: %v", err)
			panic(err)
		}
		e.dismissQuotaUI(p)
	}()
	if err := e.quotaClick(p, jsQuotaSettings, "找不到设置入口（Settings／設定／设置）"); err != nil {
		return nil, err
	}
	if err := e.quotaClick(p, jsQuotaMenu, "设置菜单未出现可识别的设置项"); err != nil {
		return nil, err
	}
	deadline := e.now().Add(12 * time.Second)
	for {
		v, err := p.JS(jsQuotaDialog, false, 0)
		if err != nil {
			return nil, err
		}
		parsed := parseQuota(v.Str2())
		if found, _ := parsed["found"].(bool); found {
			return parsed, nil
		}
		if !e.now().Before(deadline) {
			return nil, &ErrGeneration{Msg: "额度面板未读到有效用量；旧缓存未覆盖"}
		}
		e.sleep(200 * time.Millisecond)
	}
}

// quotaClick 对应 _quota_click()：轮询入口按钮，找到就点一下。
func (e *Engine) quotaClick(p Session, expr, errMsg string) error {
	deadline := e.now().Add(8 * time.Second)
	for {
		raw, err := p.JS(expr, false, 0)
		if err != nil {
			return err
		}
		if raw.Truthy() {
			pt, perr := readPoint(raw.Str2())
			if perr != nil {
				return &ErrGeneration{Msg: perr.Error()}
			}
			e.clickPoint(p, pt[0], pt[1])
			return nil
		}
		if !e.now().Before(deadline) {
			return &ErrGeneration{Msg: errMsg}
		}
		e.sleep(150 * time.Millisecond)
	}
}

// dismissQuotaUI 对应 _dismiss_quota_ui()：清掉残留的菜单和对话框。
func (e *Engine) dismissQuotaUI(p Session) {
	for i := 0; i < 2; i++ {
		for _, t := range [2]string{"keyDown", "keyUp"} {
			if _, err := p.Send("Input.dispatchKeyEvent", map[string]any{
				"type": t, "key": "Escape", "code": "Escape",
				"windowsVirtualKeyCode": 27, "nativeVirtualKeyCode": 27}, 0); err != nil {
				panic(err)
			}
		}
		e.sleep(100 * time.Millisecond)
	}
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
	planRe = regexp.MustCompile(`(?i)^.{1,60}?(?:\s+plan|計劃|计划|方案)$`)
	weeklyRe = regexp.MustCompile(`(?i)Weekly limit resets? on ([^\n]+)`)
	weeklyReZh = regexp.MustCompile(
		`(?i)(?:每(?:週|周|星期)|週|周)(?:上限|限額|限额|額度|额度)(?:會|会|將|将)?(?:於|于|在)\s*([^\n]+?)\s*(?:重設|重置)`)
	extraAtRe = regexp.MustCompile(`(?i)^(?:(?:Additional|Extra)\s+tokens|(?:額外|额外)(?:的)?(?:符元|符記|代幣|代币|令牌|憑證|凭证|tokens))`)
	pctUsedEn = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*%\s*(?:used\b|已(?:使)?用)`)
	pctUsedZh = regexp.MustCompile(`已(?:使)?用\s*(\d+(?:\.\d+)?)\s*%`)
	neverRe   = regexp.MustCompile(`(?i)Never expires|永不過期|永不过期`)
)

// parseQuota 对应 _parse_quota()。
func parseQuota(txt string) map[string]any {
	normalized := uninorm.NFKC.String(txt)
	var lines []string
	for _, ln := range strings.Split(normalized, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	// 避开 Usage 段之前的账号/资料文本。
	start := 0
	for i, ln := range lines {
		switch strings.ToLower(ln) {
		case "usage", "用量", "使用量":
			start = i + 1
		}
		if start != 0 {
			break
		}
	}
	lines = lines[start:]
	end := len(lines)
	for i, ln := range lines {
		switch strings.ToLower(ln) {
		case "language", "语言", "語言":
			end = i
		}
		if end != len(lines) {
			break
		}
	}
	lines = lines[:end]
	out := map[string]any{"raw": strings.Join(lines, "\n")}
	for _, ln := range lines {
		if planRe.MatchString(ln) {
			out["plan"] = ln
			break
		}
	}
	text := strings.Join(lines, "\n")
	if m := weeklyRe.FindStringSubmatch(text); m != nil {
		out["weekly_reset"] = strings.TrimSpace(m[1])
	} else if m := weeklyReZh.FindStringSubmatch(text); m != nil {
		out["weekly_reset"] = strings.TrimSpace(m[1])
	}

	extraAt := len(lines)
	for i, ln := range lines {
		if extraAtRe.MatchString(ln) {
			extraAt = i
			break
		}
	}

	percentage := func(line string) any {
		m := pctUsedEn.FindStringSubmatch(line)
		if m == nil {
			m = pctUsedZh.FindStringSubmatch(line)
		}
		if m == nil {
			return nil
		}
		value, perr := strconv.ParseFloat(m[1], 64)
		if perr != nil || value < 0 || value > 100 {
			return nil
		}
		if value == float64(int(value)) {
			return int(value)
		}
		return value
	}
	for _, ln := range lines[:extraAt] {
		if v := percentage(ln); v != nil {
			out["weekly_used_pct"] = v
			break
		}
	}
	for _, ln := range lines[extraAt:] {
		if v := percentage(ln); v != nil {
			out["extra_used_pct"] = v
			if m := regexp.MustCompile(`\(([^)]+)\)`).FindStringSubmatch(ln); m != nil {
				out["extra_left"] = strings.TrimSpace(m[1])
			}
			break
		}
	}
	if neverRe.MatchString(text) {
		out["extra_expires"] = "never"
	}
	// 只认到用量百分比才算读成功：只有 plan 文本不够，保留旧缓存。
	_, out["found"] = out["weekly_used_pct"]
	return out
}
