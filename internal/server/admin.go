package server

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"muse2api-go/internal/store"

	"muse2api-go/internal/pyval"
	"muse2api-go/internal/webui"
)

// admin.go 对应 app.py 的「管理面」：总览、账号池、接入信息、任务与媒体、静态页面与扩展。
// 错误壳一律 {"detail": ...}（面板就是按 detail 取信息的）。
// 需要浏览器的几条（test / relogin / quota / keepalive 触发）在下一个提交里补。

func (a *App) registerAdmin() {
	a.add("GET", "/admin/status", true, a.adminStatus)
	a.add("GET", "/admin/accounts", true, a.listAccounts)
	a.add("POST", "/admin/accounts", true, a.addAccount)
	a.add("PATCH", "/admin/accounts/{aid}", true, a.patchAccount)
	a.add("DELETE", "/admin/accounts/{aid}", true, a.delAccount)
	a.add("POST", "/admin/accounts/{aid}/cookies", true, a.updateCookies)
	a.add("GET", "/admin/apikey", true, a.getApikey)
	a.add("POST", "/admin/apikey/rotate", true, a.rotateApikey)
	a.add("GET", "/admin/tasks", true, a.adminTasks)
	a.add("DELETE", "/admin/tasks/{tid}", true, a.delTask)
	a.add("POST", "/admin/tasks/clear", true, a.clearTasks)
	a.add("GET", "/admin/media", true, a.adminMedia)
	a.add("GET", "/admin/cookie-helper", false, a.cookieHelper)
	a.add("GET", "/admin/extension", false, a.extensionZip)
	a.add("GET", "/admin/extension/files", false, a.extensionFiles)
	a.add("GET", "/admin/keepalive/status", true, a.keepaliveStatus)
	a.add("GET", "/admin/update/check", false, a.checkUpdate)
	a.add("GET", "/admin/repo/status", false, a.checkUpdate)
	a.add("GET", "/", false, a.indexPage)
	a.add("POST", "/admin/update/upgrade", true, a.upgradeManual)
	a.add("POST", "/admin/repo/pull", true, a.upgradeManual)
	a.add("POST", "/admin/repo/push", true, a.repoPushManual)
	a.add("GET", "/admin", false, a.indexPage)
	a.registerBrowserAdmin()
}

// --- 总览 ---

func (a *App) adminStatus(c *reqCtx) (any, error) {
	base := a.publicBase()
	mediaCount := 0
	if entries, err := os.ReadDir(a.CFG.MediaDir()); err == nil {
		mediaCount = len(entries)
	}
	return pyval.Obj{
		{"accounts", a.Store.ListAccounts()},
		{"stats", a.Store.Stats()},
		{"tasks", a.Store.ListTasks(20)},
		{"media_count", mediaCount},
		{"browser_running", a.Engine.BrowserProcUp()},
		{"essential_cookies", essentialCookiesList()},
		{"base_url", urlOrEmpty(base, "/v1")},
		{"config", pyval.Obj{
			{"site", a.CFG.SiteURL}, {"cdp_port", a.CFG.CdpPort},
			{"image_timeout", a.CFG.ImageTimeout}, {"video_timeout", a.CFG.VideoTimeout},
			{"host", a.CFG.Host}, {"port", a.CFG.Port}, {"public_base", base},
		}},
	}, nil
}

func urlOrEmpty(base, suffix string) string {
	if base == "" {
		return ""
	}
	return base + suffix
}

func (a *App) listAccounts(c *reqCtx) (any, error) {
	return pyval.Obj{
		{"accounts", a.Store.ListAccounts()}, {"stats", a.Store.Stats()},
	}, nil
}

// --- 账号池：导入 / 改标签 / 删除 / 补 cookie ---

type batchLine struct {
	label   string
	cookies map[string]string
}

func (a *App) addAccount(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	label := strOf(body["label"])
	cookieHdr := strOrNull(body, "cookie_header", nil)
	batch := strOrNull(body, "batch", nil)
	cookies := toStringMap(body["cookies"])
	expires := map[string]int{}
	if m, ok := body["expires"].(map[string]any); ok {
		for k, v := range m {
			if pos(v) {
				expires[k] = int(numOr(v))
			}
		}
	}

	var added []any
	var seen []map[string]string
	if batch != nil && *batch != "" {
		for _, bl := range parseBatch(*batch) {
			acc := a.Store.AddAccount(toStringAny(bl.cookies), bl.label, nil)
			seen = append(seen, bl.cookies)
			added = append(added, addedEntry(acc, len(bl.cookies)))
		}
	}
	if cookieHdr != nil && *cookieHdr != "" {
		for k, v := range parseCookieText(*cookieHdr) {
			cookies[k] = v
		}
	}
	if len(cookies) > 0 {
		exp := map[string]any{}
		for k, v := range expires {
			exp[k] = v
		}
		acc := a.Store.AddAccount(toStringAny(cookies), label, exp)
		seen = append(seen, cookies)
		added = append(added, addedEntry(acc, len(cookies)))
	}
	if len(added) == 0 {
		return nil, httpError(400, "未解析到任何 cookie，请检查格式")
	}
	// 只要有一个账号把核心 cookie 凑齐就算通过（批量导入按整体判断）。
	var missing []string
	for _, n := range essentialCookiesList() {
		ok := false
		for _, set := range seen {
			if _, has := set[n]; has {
				ok = true
			}
		}
		if !ok {
			missing = append(missing, n)
		}
	}
	warning := ""
	if len(missing) > 0 {
		warning = "缺少核心 cookie：" + strings.Join(missing, ", ") + "，该账号可能无法生成"
	}
	return pyval.Obj{
		{"added", added}, {"count", len(added)},
		{"essential_missing", orEmptyList(missing)},
		{"warning", warning},
	}, nil
}

func addedEntry(acc map[string]any, n int) pyval.Obj {
	if acc == nil {
		acc = map[string]any{}
	}
	return pyval.Obj{{"id", acc["id"]}, {"label", acc["label"]},
		{"cookie_count", n}, {"expires_at", acc["expires_at"]}}
}

func orEmptyList(l []string) []any {
	out := make([]any, 0, len(l))
	for _, s := range l {
		out = append(out, s)
	}
	return out
}

func toStringAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var cookieSplitRE = regexp.MustCompile(`[;\n]+`)

// parseCookieText 对应 parse_cookie_text()：`a=1; b=2`、JSON 对象、Set-Cookie 行都能吃。
func parseCookieText(text string) map[string]string {
	out := map[string]string{}
	text = strings.TrimSpace(text)
	if text == "" {
		return out
	}
	if strings.HasPrefix(text, "{") {
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err == nil {
			for k, v := range obj {
				out[k] = strOf(v)
			}
			return out
		}
	}
	for _, part := range cookieSplitRE.Split(text, -1) {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// parseBatch 对应 parse_batch()：每行 `标签 | cookie串`，标签可省略；# 开头是注释。
func parseBatch(text string) []batchLine {
	var out []batchLine
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		label, body := "", line
		if i := strings.Index(line, "|"); i >= 0 {
			head := strings.TrimSpace(line[:i])
			if !strings.Contains(head, "=") { // `|` 出现在 cookie 里时不算分隔符
				label, body = head, strings.TrimSpace(line[i+1:])
			}
		}
		if cookies := parseCookieText(body); len(cookies) > 0 {
			out = append(out, batchLine{label: label, cookies: cookies})
		}
	}
	return out
}

func (a *App) patchAccount(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	kw := map[string]any{}
	if v, ok := body["label"]; ok {
		kw["label"] = v
	}
	if v, ok := body["enabled"]; ok {
		kw["enabled"] = v
	}
	acc := a.Store.UpdateAccount(c.Args["aid"], kw)
	if acc == nil {
		return nil, httpError(404, "账号不存在")
	}
	return accountSansCookies(acc), nil
}

// accountSansCookies 对应 Python 的 {k: v for k, v in acc.items() if k != "cookies"} | {...}：
// 摘掉 cookie 本体，只留 cookie_count。
func accountSansCookies(acc map[string]any) pyval.Obj {
	n := 0
	if m, ok := acc["cookies"].(map[string]any); ok {
		n = len(m)
	}
	keys := make([]string, 0, len(acc))
	for k := range acc {
		if k != "cookies" {
			keys = append(keys, k)
		}
	}
	// Python 保留 dict 插入序；Go map 无序，这里按字母排（偏离清单第 6 条）。
	sort.Strings(keys)
	out := make(pyval.Obj, 0, len(keys)+1)
	for _, k := range keys {
		out = append(out, pyval.Pair{Key: k, Val: acc[k]})
	}
	return append(out, pyval.Pair{Key: "cookie_count", Val: n})
}

func (a *App) delAccount(c *reqCtx) (any, error) {
	if !a.Store.DeleteAccount(c.Args["aid"]) {
		return nil, httpError(404, "账号不存在")
	}
	return pyval.Obj{{"deleted", true}, {"id", c.Args["aid"]}}, nil
}

func (a *App) updateCookies(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	cookies := toStringMap(body["cookies"])
	if hdr, ok := body["cookie_header"].(string); ok && hdr != "" {
		for k, v := range parseCookieText(hdr) {
			cookies[k] = v
		}
	}
	if len(cookies) == 0 {
		return nil, httpError(400, "未解析到 cookie")
	}
	exp := map[string]any{}
	if m, ok := body["expires"].(map[string]any); ok {
		for k, v := range m {
			if pos(v) {
				exp[k] = int(numOr(v))
			}
		}
	}
	kw := map[string]any{
		"cookies": cookies, "note": "已更新 cookie",
		"expiry_anchor": a.now().Unix(),
	}
	if len(exp) > 0 {
		kw["cookies_exp"] = exp
	}
	// 补入的是「全新会话」的 cookie，有效期估算锚点必须重置到当下，
	// 否则沿用旧锚点会把剩余天数算少。
	acc := a.Store.UpdateAccount(c.Args["aid"], kw)
	if acc == nil {
		return nil, httpError(404, "账号不存在")
	}
	return pyval.Obj{{"ok", true}, {"id", c.Args["aid"]},
		{"cookie_count", len(cookies)}, {"expires_at", acc["expires_at"]}}, nil
}

// --- 接入信息 / API Key ---

func (a *App) getApikey(c *reqCtx) (any, error) {
	base := a.publicBase()
	return pyval.Obj{
		{"api_key", a.CFG.APIKey}, {"base_url", urlOrEmpty(base, "/v1")},
		{"models_url", urlOrEmpty(base, "/v1/models")},
		{"media_url", urlOrEmpty(base, "/v1/media/{name}")},
	}, nil
}

func (a *App) rotateApikey(c *reqCtx) (any, error) {
	newKey := "m2a_" + randHex(48)
	old := a.CFG.APIKey
	a.mu.Lock()
	a.CFG.APIKey = newKey
	a.mu.Unlock()
	a.persistEnv("MUSE2API_KEY", newKey)
	return pyval.Obj{{"ok", true}, {"api_key", newKey}, {"previous", old},
		{"message", "已生成新 Key 并立即生效；旧 Key 已失效，请更新下游项目"}}, nil
}

// persistEnv 把配置写回 .env：保留其它行，原子替换（与 Python 同一条路径）。
func (a *App) persistEnv(key, value string) {
	path := filepath.Join(a.CFG.BaseDir, ".env")
	raw, err := os.ReadFile(path)
	lines := []string{}
	if err == nil {
		lines = strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}
	found := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), key+"=") {
			lines[i] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	tmp := path + ".tmp"
	body := strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
	if os.WriteFile(tmp, []byte(body), 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// --- 任务与媒体 ---

func (a *App) adminTasks(c *reqCtx) (any, error) {
	limit := 50
	if v := c.R.URL.Query().Get("limit"); v != "" {
		n, err := parseIntStrict(v)
		if err != nil {
			return nil, a.validationFailed(c.R, []vErr{{
				Type: "int_parsing", Loc: []any{"query", "limit"},
				Msg:   "Input should be a valid integer, unable to parse string as an integer",
				Input: v}})
		}
		limit = int(n)
	}
	return pyval.Obj{{"tasks", a.Store.ListTasks(limit)}}, nil
}

func (a *App) delTask(c *reqCtx) (any, error) {
	if !a.Store.DeleteTask(c.Args["tid"]) {
		return nil, httpError(404, "任务不存在")
	}
	return pyval.Obj{{"deleted", true}}, nil
}

func (a *App) clearTasks(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	keep := 0
	if v, ok := body["keep"]; ok && truthy(v) {
		keep = int(numOr(v))
	}
	return pyval.Obj{{"removed", a.Store.ClearTasks(keep)}}, nil
}

func (a *App) adminMedia(c *reqCtx) (any, error) {
	type item struct {
		name  string
		url   string
		bytes int64
		mtime int64
		kind  string
	}
	var items []item
	if entries, err := os.ReadDir(a.CFG.MediaDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			st, err := e.Info()
			if err != nil {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			kind := "image"
			if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
				kind = "video"
			}
			items = append(items, item{name: e.Name(), url: a.mediaURL(e.Name()),
				bytes: st.Size(), mtime: st.ModTime().Unix(), kind: kind})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].mtime > items[j].mtime })
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, pyval.Obj{{"name", it.name}, {"url", it.url},
			{"bytes", it.bytes}, {"mtime", it.mtime}, {"kind", it.kind}})
	}
	return pyval.Obj{{"media", out}, {"count", len(out)}}, nil
}

// --- 静态页面与取 cookie 的产物 ---

func (a *App) indexPage(c *reqCtx) (any, error) {
	return &rawResponse{Status: http.StatusOK,
		Headers: map[string]string{"content-type": "text/html; charset=utf-8"},
		Body:    webui.AdminHTML()}, nil
}

func (a *App) cookieHelper(c *reqCtx) (any, error) {
	body := webui.CookieHelper()
	hdrs := map[string]string{"content-type": "text/x-python; charset=utf-8"}
	if c.R.URL.Query().Get("download") != "" && c.R.URL.Query().Get("download") != "0" {
		hdrs["content-disposition"] = `attachment; filename="get_muse_cookie.py"`
	}
	return &rawResponse{Status: http.StatusOK, Headers: hdrs, Body: body}, nil
}

func (a *App) extensionFiles(c *reqCtx) (any, error) {
	list := webui.ExtensionFiles()
	if list == nil {
		return nil, httpError(404, "扩展目录缺失")
	}
	out := make([]any, 0, len(list))
	for _, n := range list {
		out = append(out, n)
	}
	return pyval.Obj{{"files", out}}, nil
}

func (a *App) extensionZip(c *reqCtx) (any, error) {
	list := webui.ExtensionFiles()
	if list == nil {
		return nil, httpError(404, "扩展目录缺失")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range list {
		data, ok := webui.ExtensionFile(name)
		if !ok {
			continue
		}
		w, err := zw.Create("muse2api-extension/" + name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return &rawResponse{Status: http.StatusOK, Headers: map[string]string{
		"content-type":        "application/zip",
		"content-disposition": `attachment; filename="muse2api-extension.zip"`,
		"cache-control":       "no-store",
	}, Body: buf.Bytes()}, nil
}

// --- 保活状态（只读；触发那几条要等浏览器版路由一起补） ---

func (a *App) keepaliveStatus(c *reqCtx) (any, error) {
	a.kaMu.Lock()
	defer a.kaMu.Unlock()
	return pyval.Obj{
		{"running", a.kaState.Running}, {"last_run", a.kaState.LastRun},
		{"last_result", a.kaState.LastResult}, {"next_run", a.kaState.NextRun},
	}, nil
}

// randHex 对应 Python 的 secrets.token_hex(n)：n 字节随机数的十六进制（2n 个字符）。
func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func essentialCookiesList() []string {
	return append([]string(nil), store.EssentialCookies...)
}
