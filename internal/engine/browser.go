// Package engine 是 Muse 生成引擎：用真实网页会话驱动 muse.ai 完成生图/生视频/对话。
// 逐方法对应 Python 的 engine.py；注入的 JS 见 js.go（由 AST 金样本生成）。
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"muse2api-go/internal/config"
)

// 决定账号生死的核心 cookie（缺失或过期 = 会话失效）。与 store 保持一致，见测试。
var essentialCookies = []string{"hatch_sess", "hatch_gw", "hatch_vml", "hatch_native_auth_device"}

var loginRe = regexp.MustCompile(`log in|sign in|create an account|登录|use another account`)

// SessionFactory 建立一条 CDP 会话；测试里换成假实现。
type SessionFactory func(wsURL string, timeout time.Duration) (Session, error)

// Process 是被启动的浏览器进程，测试里换成假的。
type Process interface {
	Alive() bool
	Terminate()
	Kill()
}

// Launcher 对应 subprocess.Popen：给定 argv/env/cwd/日志句柄返回进程句柄。
type Launcher func(args, env []string, dir string, logFile *os.File) (Process, error)

type Engine struct {
	CFG  *config.Config
	HTTP HTTPClient
	New  SessionFactory
	// Launch 是进程启动注入点；nil 时用真实的 exec.Command。
	Launch Launcher

	proc          Process
	browser       Session
	page          Session
	currentAccID  string
	lastHTTPRenew map[string]float64
	logFile       *os.File

	now         func() time.Time
	sleep       func(time.Duration)
	waitTimeout time.Duration
}

func NewEngine(cfg *config.Config, httpClient HTTPClient, newSession SessionFactory) *Engine {
	if newSession == nil {
		newSession = defaultSessionFactory
	}
	return &Engine{
		CFG:           cfg,
		HTTP:          httpClient,
		New:           newSession,
		lastHTTPRenew: map[string]float64{},
		now:           time.Now,
		sleep:         time.Sleep,
		waitTimeout:   10 * time.Second,
	}
}

func defaultSessionFactory(wsURL string, timeout time.Duration) (Session, error) {
	c, err := newRealSession(wsURL, timeout)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (e *Engine) debugURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/json/version", e.CFG.CdpPort)
}

func (e *Engine) listURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/json/list", e.CFG.CdpPort)
}

func (e *Engine) profileDir() string { return e.CFG.ProfileDir() }

// chromiumArgs 复刻 engine.py:63-74 的参数表；顺序即 Python 的顺序，测试逐条比对。
func (e *Engine) chromiumArgs() []string {
	return []string{
		e.CFG.Chromium,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--disable-dev-shm-usage", "--disable-background-networking",
		"--no-first-run", "--no-default-browser-check",
		"--autoplay-policy=no-user-gesture-required",
		"--window-size=1440,2400",
		fmt.Sprintf("--remote-debugging-port=%d", e.CFG.CdpPort),
		"--remote-allow-origins=*",
		fmt.Sprintf("--user-data-dir=%s", e.profileDir()),
		"about:blank",
	}
}

// Start 对应 start()：必要时复用已有 CDP，否则清锁、起进程、最多等 90 秒。
func (e *Engine) Start() error {
	if e.proc != nil && e.alive() && e.browser != nil {
		return nil
	}

	// 尝试复用已有健康 CDP（Python 里只在 proc 为空时做）
	if e.proc == nil {
		if v, err := e.httpJSON(e.debugURL(), time.Second); err == nil {
			if ws, ok := v["webSocketDebuggerUrl"]; ok {
				if b, err := e.New(asString(ws), 180*time.Second); err == nil {
					e.browser = b
					return nil
				}
			}
		}
	}

	// 清理残留锁（悬空符号链接也要删，所以用 Lstat）
	for _, lockName := range [3]string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		lp := filepath.Join(e.profileDir(), lockName)
		if _, err := os.Lstat(lp); err == nil {
			_ = os.Remove(lp)
		}
	}

	if err := os.MkdirAll(e.CFG.DataDir(), 0o755); err != nil {
		return &ErrGeneration{Msg: err.Error()}
	}
	lf, err := os.OpenFile(filepath.Join(e.CFG.DataDir(), "chromium.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return &ErrGeneration{Msg: err.Error()}
	}
	e.logFile = lf

	launch := e.Launch
	if launch == nil {
		launch = execLauncher
	}
	proc, err := launch(e.chromiumArgs(), e.childEnv(), e.childCWD(), lf)
	if err != nil {
		_ = lf.Close()
		e.logFile = nil
		return &ErrGeneration{Msg: err.Error()}
	}
	e.proc = proc

	var last error
	for i := 0; i < 90; i++ {
		v, err := e.httpJSON(e.debugURL(), 2*time.Second)
		if err == nil {
			ws, ok := v["webSocketDebuggerUrl"]
			if !ok {
				err = errors.New("webSocketDebuggerUrl 缺失")
			} else if b, err2 := e.New(asString(ws), 180*time.Second); err2 != nil {
				err = err2
			} else {
				e.browser = b
				return nil
			}
		}
		last = err
		e.sleep(time.Second)
	}
	return &ErrGeneration{Msg: fmt.Sprintf("Chromium 启动失败: %v", last)}
}

func (e *Engine) alive() bool {
	return e.proc != nil && e.proc.Alive()
}

// childCWD 对应 Python 的 cwd_dir：home_dir 存在且是目录才设，否则不设。
func (e *Engine) childCWD() string {
	d := e.CFG.HomeDir
	if d == "" {
		return ""
	}
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

// waitProcExit 轮询等待进程退出，对应 proc.wait(timeout=...)。
func waitProcExit(p Process, timeout time.Duration, sleep func(time.Duration)) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !p.Alive() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		sleep(20 * time.Millisecond)
	}
}

// execLauncher 与 execProcess 由 launcher_unix.go / launcher_windows.go 提供
// （SIGTERM 在 Windows 上不存在，所以用构建标签分流）。

// childEnv 对应 env=dict(os.environ) + HOME setdefault + extra_path 前置。
func (e *Engine) childEnv() []string {
	m := map[string]string{}
	order := []string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, dup := m[k]; !dup {
			order = append(order, k)
		}
		m[k] = v
	}
	if _, ok := m["HOME"]; !ok {
		m["HOME"] = e.CFG.HomeDir
		order = append(order, "HOME")
	}
	if e.CFG.ExtraPath != "" {
		m["PATH"] = e.CFG.ExtraPath + string(os.PathListSeparator) + m["PATH"]
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

// Stop 对应 stop()：先关会话再终止进程，10 秒兜底 KILL。
func (e *Engine) Stop() {
	for _, c := range []Session{e.page, e.browser} {
		if c != nil {
			c.Close()
		}
	}
	e.page, e.browser = nil, nil

	if e.proc != nil && e.alive() {
		e.proc.Terminate() // proc.terminate()
		timeout := e.waitTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		if !waitProcExit(e.proc, timeout, e.sleep) {
			e.proc.Kill() // proc.kill()
			waitProcExit(e.proc, 5*time.Second, e.sleep)
		}
	}
	e.proc = nil
	if e.logFile != nil {
		_ = e.logFile.Close()
		e.logFile = nil
	}
}

// openPage 对应 _open_page()：关掉现存 page target，开新页面并 enable 三个域。
func (e *Engine) openPage() (Session, error) {
	if resp, err := e.HTTP.Get(e.listURL(), nil, 3*time.Second, true); err == nil {
		var items []map[string]any
		if json.Unmarshal(resp.Body, &items) == nil {
			for _, it := range items {
				if it["type"] == "page" {
					if id, ok := it["id"].(string); ok && id != "" {
						_, _ = e.HTTP.Get(fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", e.CFG.CdpPort, id),
							nil, 2*time.Second, true)
					}
				}
			}
		}
	}

	resp, err := e.HTTP.Put(fmt.Sprintf("http://127.0.0.1:%d/json/new?about:blank", e.CFG.CdpPort),
		nil, 10*time.Second)
	if err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	var tgt map[string]any
	if err := json.Unmarshal(resp.Body, &tgt); err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	page, err := e.New(asString(tgt["webSocketDebuggerUrl"]), 180*time.Second)
	if err != nil {
		return nil, &ErrGeneration{Msg: err.Error()}
	}
	for _, m := range []struct {
		method string
		params map[string]any
	}{
		{"Network.enable", nil},
		{"Page.enable", nil},
		{"Runtime.enable", nil},
		{"Browser.setDownloadBehavior", map[string]any{"behavior": "allow", "downloadPath": e.CFG.DownloadDir()}},
	} {
		if _, err := page.Send(m.method, m.params, 0); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// ApplyCookies 对应 _apply_cookies()：彻底清空旧账号 cookie，且绝不传入过去时间的 expires，
// 否则 Chromium 会丢掉 hatch_vml。
//
// DEVIATION: Python 按 dict 插入顺序遍历，Go 的 map 无序 —— 这里按名字排序遍历。
// 每个 cookie 是独立的 setCookie 调用，顺序不影响最终 cookie jar。
func (e *Engine) ApplyCookies(page Session, cookies map[string]string, expires map[string]int) {
	_, _ = page.Send("Network.clearBrowserCookies", nil, 0)
	now := float64(e.now().UnixNano()) / 1e9
	names := make([]string, 0, len(cookies))
	for k := range cookies {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		value := cookies[name]
		if value == "" {
			continue
		}
		expVal := now + 7*86400
		if exp, ok := expires[name]; ok && exp != 0 && float64(exp) > now+3600 {
			expVal = float64(exp)
		}
		for _, dom := range [2]string{".muse.ai", "muse.ai"} {
			_, _ = page.Send("Network.setCookie", map[string]any{
				"name":    name,
				"value":   value,
				"domain":  dom,
				"path":    "/",
				"secure":  true,
				"expires": expVal,
			}, 0)
		}
	}
}

// ReadCookies 对应 read_cookies()：含 httpOnly，这是页面 JS 做不到的。
func (e *Engine) ReadCookies() map[string]CookieEntry {
	if e.page == nil {
		return map[string]CookieEntry{}
	}
	msg, err := e.page.Send("Network.getCookies", map[string]any{"urls": []string{e.CFG.SiteURL}}, 20*time.Second)
	if err != nil {
		return map[string]CookieEntry{}
	}
	out := map[string]CookieEntry{}
	result, _ := msg["result"].(map[string]any)
	list, _ := result["cookies"].([]any)
	for _, raw := range list {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := asString(c["name"])
		if name == "" {
			continue
		}
		exp := int64(-1)
		if v, has := c["expires"]; has {
			if f, err := strconv.ParseFloat(asString(v), 64); err == nil {
				exp = int64(f)
			}
		}
		out[name] = CookieEntry{Value: asString(c["value"]), Expires: exp}
	}
	return out
}

// waitWSReady 等待 muse.ai 完成 React hydration 且不再处于 Connecting... 状态。
func (e *Engine) waitWSReady(page Session, timeout time.Duration) bool {
	deadline := e.now().Add(timeout)
	for e.now().Before(deadline) {
		st, err := page.JS(jsWaitWsReadyL272, false, 0)
		if err == nil && st.Str2() == "ready" {
			return true
		}
		e.sleep(80 * time.Millisecond)
	}
	return false
}

// ResetThread 关闭残留弹窗并确保处于干净会话且 WebSocket 已就绪。
// 纯文本对话（forChat=true）时，若热页面无附件、无卡死且气泡不多，直接复用热连接以实现秒回。
func (e *Engine) ResetThread(forChat bool) {
	if e.page == nil {
		return
	}
	arg := "false"
	if forChat {
		arg = "true"
	}
	expr, err := pyFormat(jsResetThreadL294, arg)
	if err != nil {
		log.Printf("reset_thread JS 格式化失败: %v", err)
		return
	}
	needsNav, err := e.page.JS(expr, false, 0)
	if err != nil {
		return
	}
	if !needsNav.Truthy() {
		return
	}
	_, _ = e.page.Send("Page.navigate", map[string]any{"url": "https://muse.ai/thread/new"}, 0)
	end := e.now().Add(10 * time.Second)
	for e.now().Before(end) {
		e.sleep(80 * time.Millisecond)
		ready, err := e.page.JS(jsResetThreadL319, false, 0)
		if err == nil && ready.Truthy() {
			break
		}
	}
	e.waitWSReady(e.page, 12*time.Second)
}

// EnsurePage 保证有一个登录态正确的热页面；必要时先纯 HTTP 续签再开页。
func (e *Engine) EnsurePage(cookies map[string]string, expires map[string]int, accountID string) (Session, error) {
	if e.page != nil && (accountID == "" || e.currentAccID == accountID) {
		if ok, err := e.page.JS(jsEnsurePageL377, false, 0); err == nil && ok.Truthy() {
			return e.page, nil
		}
	}
	if e.page != nil {
		e.page.Close()
		e.page = nil
	}

	// 仅当距离上次 HTTP 续签超过 10 分钟才在主链路打 /api/session，避免每次切号重复阻塞
	nowTS := float64(e.now().UnixNano()) / 1e9
	if accountID == "" || nowTS-e.lastHTTPRenew[accountID] > 600 {
		renewed, err := e.RenewSessionHTTP(cookies, expires, true)
		if err != nil {
			if IsAuthErr(err) {
				return nil, err
			}
			log.Printf("预续签 /api/session 失败（继续尝试浏览器加载）: %v", err)
		} else {
			if len(renewed.Cookies) > 0 {
				cookies = renewed.Cookies
			}
			if len(renewed.CookiesExp) > 0 {
				expires = renewed.CookiesExp
			}
			if accountID != "" {
				e.lastHTTPRenew[accountID] = nowTS
			}
		}
	}

	page, err := e.openPage()
	if err != nil {
		return nil, err
	}
	e.ApplyCookies(page, cookies, expires)
	if _, err := page.Send("Page.navigate", map[string]any{"url": "https://muse.ai/thread/new"}, 0); err != nil {
		return nil, err
	}
	for i := 0; i < e.CFG.LoginWait*2; i++ {
		e.sleep(150 * time.Millisecond)
		if ok, err := page.JS(jsEnsurePageL377, false, 0); err == nil && ok.Truthy() {
			e.page = page
			e.currentAccID = accountID
			e.waitWSReady(page, 15*time.Second)
			return page, nil
		}
	}
	if ok, err := page.JS(jsEnsurePageL377, false, 0); err == nil && ok.Truthy() {
		e.page = page
		e.currentAccID = accountID
		e.waitWSReady(page, 15*time.Second)
		return page, nil
	}

	// 区分「会话失效（被踢回登录页）」与「页面加载卡住」，错误提示才能对症
	body := ""
	if v, err := page.JS(jsEnsurePageL386, false, 0); err == nil {
		body = strings.ToLower(v.Str2())
	}
	page.Close()
	if loginRe.MatchString(body) {
		return nil, &ErrAuth{Msg: "会话已被 muse.ai 登出（可能被其它登录挤掉或触发风控），" +
			"请用浏览器扩展重新导入 cookie"}
	}
	return nil, &ErrGeneration{Msg: "muse.ai 页面加载超时（未出现聊天输入框），请检查服务器网络后重试；未确认会话失效"}
}

// Refresh 对应 refresh()。
func (e *Engine) Refresh(cookies map[string]string, expires map[string]int) (Session, error) {
	if e.page != nil {
		e.page.Close()
		e.page = nil
	}
	return e.EnsurePage(cookies, expires, "")
}

// CurrentAccountID / Page / Browser 暴露内部状态（app.py 直接读这些属性）。
func (e *Engine) CurrentAccountID() string { return e.currentAccID }
func (e *Engine) Page() Session            { return e.page }
func (e *Engine) BrowserProcUp() bool      { return e.proc != nil && e.alive() }

func (e *Engine) httpJSON(url string, timeout time.Duration) (map[string]any, error) {
	resp, err := e.HTTP.Get(url, nil, timeout, true)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
