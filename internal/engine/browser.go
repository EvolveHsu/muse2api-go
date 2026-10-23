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
	"sync"
	"sync/atomic"
	"time"

	"muse2api-go/internal/config"
)

// 决定账号生死的核心 cookie（缺失或过期 = 会话失效）。与 store 保持一致，见测试。
var essentialCookies = []string{"hatch_sess", "hatch_vml", "hatch_native_auth_device"}

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
	// Fetch 是参考图下载注入点；nil 时用 liveFetch。
	Fetch fetchURL
	// EnsurePageFn 是"取到热页面"这一步的注入点：Go 没有 monkeypatch，
	// 而金样本录制时这一步是被打桩跳过的。nil 时用真实的 EnsurePage。
	EnsurePageFn func(cookies map[string]string, expires map[string]int, accountID string) (Session, error)

	// startMu 对应上游 1.6.0 给 start()/stop() 加的 _start_lock：
	// 多任务并发时 Start 可能被多个 goroutine 同时调用，不能各自拉一个 Chromium。
	startMu sync.Mutex
	// contextsMu / activeContexts 对应 _contexts_lock / _active_contexts：
	// 每个并发任务各自持有一个 Incognito BrowserContext，退出时必须全部 dispose。
	contextsMu     sync.Mutex
	activeContexts map[string]bool
	// stateMu 护住下面这几个字段的「读-改-发快照」：并发任务会同时换 page/currentAccID，
	// 而 visible 快照要求整组一致（见 DEVIATION(#16)）。
	stateMu sync.Mutex

	// DEVIATION(#16): Python 没有 visible / renewMu，它靠 GIL 保证「一次赋值/一次查表」的原子性。
	// Go 里这两个字段会被**不持 GEN_LOCK 的读者**看到：面板每几秒打 /admin/status 与 /readyz
	// （BrowserProcUp），保活协程读写 lastHTTPRenew（renewAndPersist 在锁外也走）。
	// 不加锁的并发读写在 Go 里是数据竞争，最坏 `fatal error: concurrent map writes` 直接带走进程。
	//
	// 处理方式不改任何获取顺序：proc/page/currentAccID 本体仍旧只由 GEN_LOCK 持有者读写，
	// 变更时整份替换一份 atomic 快照供锁外读者用；lastHTTPRenew 那张表则用小锁包住单次读写。
	visible      atomic.Pointer[browserState]
	renewMu      sync.Mutex
	proc         Process
	browser      Session
	page         Session
	currentAccID string

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
		CFG:            cfg,
		HTTP:           httpClient,
		New:            newSession,
		activeContexts: map[string]bool{},
		lastHTTPRenew:  map[string]float64{},
		now:            time.Now,
		sleep:          time.Sleep,
		waitTimeout:    10 * time.Second,
	}
}

// PageCtx 对应 Python engine 里 threading.local() 保存的三件套：
// page / current_acc_id / current_context_id。
// Go 没有线程局部存储：每个并发任务显式持有一个 PageCtx，一个任务一条链路，
// 语义与上游「每线程独立页面 + 独立 Incognito BrowserContext」一一对应。
type PageCtx struct {
	Page      Session
	AccID     string
	ContextID string
}

// NewPageCtx 建一个空的任务上下文（对应一个新 worker 线程的 thread-local）。
func (e *Engine) NewPageCtx() *PageCtx { return &PageCtx{} }

func defaultSessionFactory(wsURL string, timeout time.Duration) (Session, error) {
	c, err := newRealSession(wsURL, timeout)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// browserState 是给「不抢浏览器锁的读者」看的那份快照；三个字段永远整份换掉，
// 所以读者不会看见「进程已经没了但页面还挂着」这种半新半旧的组合。
type browserState struct {
	proc  Process
	page  Session
	accID string
}

// publish 在每次状态变更后调用；读本体字段前先拿 stateMu，因为并发任务都会换这些字段。
func (e *Engine) publish() {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.visible.Store(&browserState{proc: e.proc, page: e.page, accID: e.currentAccID})
}

func (e *Engine) snap() *browserState {
	if s := e.visible.Load(); s != nil {
		return s
	}
	return &browserState{}
}

func (e *Engine) setProc(p Process) {
	e.stateMu.Lock()
	e.proc = p
	e.visible.Store(&browserState{proc: e.proc, page: e.page, accID: e.currentAccID})
	e.stateMu.Unlock()
}

func (e *Engine) setBrowser(s Session) {
	e.stateMu.Lock()
	e.browser = s
	e.stateMu.Unlock()
}

func (e *Engine) browserSession() Session {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.browser
}

func (e *Engine) procHandle() Process {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.proc
}

func (e *Engine) setPage(s Session) {
	e.stateMu.Lock()
	e.page = s
	e.visible.Store(&browserState{proc: e.proc, page: e.page, accID: e.currentAccID})
	e.stateMu.Unlock()
}

func (e *Engine) setAccountID(id string) {
	e.stateMu.Lock()
	e.currentAccID = id
	e.visible.Store(&browserState{proc: e.proc, page: e.page, accID: e.currentAccID})
	e.stateMu.Unlock()
}

// renewAt / setRenewAt 包住 lastHTTPRenew 那张表的单次读写。
func (e *Engine) renewAt(accountID string) float64 {
	e.renewMu.Lock()
	defer e.renewMu.Unlock()
	return e.lastHTTPRenew[accountID]
}

func (e *Engine) setRenewAt(accountID string, ts float64) {
	e.renewMu.Lock()
	defer e.renewMu.Unlock()
	if e.lastHTTPRenew == nil {
		e.lastHTTPRenew = map[string]float64{}
	}
	e.lastHTTPRenew[accountID] = ts
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
	// 对应上游 1.6.0 的 `with self._start_lock:`：并发任务同时冷启动时只能拉一个浏览器。
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if e.procHandle() != nil && e.alive() && e.browserSession() != nil {
		return nil
	}

	// 尝试复用已有健康 CDP（Python 里只在 proc 为空时做）
	if e.procHandle() == nil {
		if v, err := e.httpJSON(e.debugURL(), time.Second); err == nil {
			if ws, ok := v["webSocketDebuggerUrl"]; ok {
				if b, err := e.New(asString(ws), 180*time.Second); err == nil {
					e.setBrowser(b)
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
	e.setProc(proc)

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
				e.setBrowser(b)
				return nil
			}
		}
		last = err
		e.sleep(time.Second)
	}
	return &ErrGeneration{Msg: fmt.Sprintf("Chromium 启动失败: %v", last)}
}

func (e *Engine) alive() bool {
	p := e.procHandle()
	return p != nil && p.Alive()
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

// Stop 对应上游 1.6.0 的 stop()：先关当前页面，再 dispose 掉所有活跃的隔离上下文，
// 关浏览器会话，最后终止进程（10 秒 SIGTERM 兜底 KILL）。
func (e *Engine) Stop() {
	e.startMu.Lock()
	defer e.startMu.Unlock()

	if p := func() Session {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		return e.page
	}(); p != nil {
		p.Close()
		e.setPage(nil)
	}
	if b := e.browserSession(); b != nil {
		for _, ctxID := range e.drainContexts() {
			_, _ = b.Send("Target.disposeBrowserContext",
				map[string]any{"browserContextId": ctxID}, 0)
		}
		b.Close()
		e.setBrowser(nil)
	}

	if p := e.procHandle(); p != nil && p.Alive() {
		p.Terminate() // proc.terminate()
		timeout := e.waitTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		if !waitProcExit(p, timeout, e.sleep) {
			p.Kill() // proc.kill()
			waitProcExit(p, 5*time.Second, e.sleep)
		}
	}
	e.setProc(nil)
	if e.logFile != nil {
		_ = e.logFile.Close()
		e.logFile = nil
	}
}

// trackContext / untrackContext / drainContexts 维护活跃 BrowserContext 集合。
func (e *Engine) trackContext(id string) {
	e.contextsMu.Lock()
	e.activeContexts[id] = true
	e.contextsMu.Unlock()
}

func (e *Engine) untrackContext(id string) {
	e.contextsMu.Lock()
	delete(e.activeContexts, id)
	e.contextsMu.Unlock()
}

func (e *Engine) drainContexts() []string {
	e.contextsMu.Lock()
	defer e.contextsMu.Unlock()
	out := make([]string, 0, len(e.activeContexts))
	for id := range e.activeContexts {
		out = append(out, id)
	}
	e.activeContexts = map[string]bool{}
	return out
}

// closeCurrentPage 对应上游 1.6.0 的 _close_page()：
// 关闭本任务页面、dispose 它自己的隔离上下文，并清空任务上下文的账号标记。
func (e *Engine) closeCurrentPage(ctx *PageCtx) {
	if ctx == nil {
		return
	}
	if p := ctx.Page; p != nil {
		p.Close()
		ctx.Page = nil
		e.stateMu.Lock()
		if e.page == p {
			e.page = nil
			e.visible.Store(&browserState{proc: e.proc, page: e.page, accID: e.currentAccID})
		}
		e.stateMu.Unlock()
	}
	if ctx.ContextID != "" {
		if b := e.browserSession(); b != nil {
			_, _ = b.Send("Target.disposeBrowserContext",
				map[string]any{"browserContextId": ctx.ContextID}, 0)
		}
		e.untrackContext(ctx.ContextID)
		ctx.ContextID = ""
	}
	ctx.AccID = ""
}

// CloseCurrentPage 是 closeCurrentPage 的公开入口（app.py 在生成失败/断开时调它清场）。
func (e *Engine) CloseCurrentPage(ctx *PageCtx) { e.closeCurrentPage(ctx) }

// enablePageDomains 是 _open_page() 两条路径共用的 4 条 domain 启用调用。
func (e *Engine) enablePageDomains(page Session) error {
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
			return err
		}
	}
	return nil
}

// openPage 对应上游 1.6.0 的 _open_page()：
// 浏览器级连接可用时，先建一个任务专属的 Incognito BrowserContext，在其中开页面
// （多账号 cookie/会话 100% 隔离）；失败则回退到旧路径（关掉现存页面后在默认上下文开新页）。
func (e *Engine) openPage(ctx *PageCtx) (Session, error) {
	if b := e.browserSession(); b != nil {
		ctxID, page, err := func() (string, Session, error) {
			res, err := b.Send("Target.createBrowserContext", nil, 0)
			if err != nil {
				return "", nil, err
			}
			result, _ := res["result"].(map[string]any)
			ctxID := asString(result["browserContextId"])
			if ctxID == "" {
				return "", nil, fmt.Errorf("Target.createBrowserContext 未返回 browserContextId")
			}
			e.trackContext(ctxID)
			tRes, err := b.Send("Target.createTarget", map[string]any{
				"url": "about:blank", "browserContextId": ctxID}, 0)
			if err != nil {
				return ctxID, nil, err
			}
			tResult, _ := tRes["result"].(map[string]any)
			targetID := asString(tResult["targetId"])
			if targetID == "" {
				return ctxID, nil, fmt.Errorf("Target.createTarget 未返回 targetId")
			}
			wsURL := fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", e.CFG.CdpPort, targetID)
			page, err := e.New(wsURL, 180*time.Second)
			if err != nil {
				return ctxID, nil, err
			}
			if err := e.enablePageDomains(page); err != nil {
				return ctxID, nil, err
			}
			return ctxID, page, nil
		}()
		if err == nil {
			ctx.ContextID = ctxID
			return page, nil
		}
		log.Printf("Target.createBrowserContext 创建隔离上下文失败，回退到普通标签页: %v", err)
	}

	// 回退：在默认上下文直接开新页面并 enable 三个域。
	// 注意：上游 1.6.0 起这里**不再**先 /json/list + /json/close 清场
	//（并发模型下清别人的页面是错的），只发一次 /json/new。
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
	if err := e.enablePageDomains(page); err != nil {
		return nil, err
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
func (e *Engine) ReadCookies(ctx *PageCtx) map[string]CookieEntry {
	if ctx == nil || ctx.Page == nil {
		return map[string]CookieEntry{}
	}
	msg, err := ctx.Page.Send("Network.getCookies", map[string]any{"urls": []string{e.CFG.SiteURL}}, 20*time.Second)
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
		st, err := page.JS(jsWaitWsReady, false, 0)
		if err == nil && st.Str2() == "ready" {
			return true
		}
		// 上游 1.6.0 把这轮轮询从 0.08 秒改成 0.1 秒（配合 textarea-disabled 判定）。
		e.sleep(100 * time.Millisecond)
	}
	return false
}

// ResetThread 关闭残留弹窗并确保处于干净会话且 WebSocket 已就绪。
// 纯文本对话（forChat=true）时，若热页面无附件、无卡死且气泡不多，直接复用热连接以实现秒回。
func (e *Engine) ResetThread(ctx *PageCtx, forChat bool) {
	if ctx == nil || ctx.Page == nil {
		return
	}
	page := ctx.Page
	arg := "false"
	if forChat {
		arg = "true"
	}
	expr, err := pyFormat(jsResetThread, arg)
	if err != nil {
		log.Printf("reset_thread JS 格式化失败: %v", err)
		return
	}
	needsNav, err := page.JS(expr, false, 0)
	if err != nil {
		return
	}
	if !needsNav.Truthy() {
		return
	}
	_, _ = page.Send("Page.navigate", map[string]any{"url": "https://muse.ai/thread/new"}, 0)
	end := e.now().Add(10 * time.Second)
	for e.now().Before(end) {
		e.sleep(80 * time.Millisecond)
		ready, err := page.JS(jsResetThreadReady, false, 0)
		if err == nil && ready.Truthy() {
			break
		}
	}
	e.waitWSReady(page, 12*time.Second)
}

// EnsurePage 保证本任务上下文有一个登录态正确的热页面；必要时先纯 HTTP 续签再开页。
// 对应上游 1.6.0 的重写：热页面复用时多等一次 _wait_ws_ready(8s)，
// 冷开页改成「先登记页面、等 login_wait、再决定成败」，失败路径显式区分
// 「网关一直 Connecting...」与「会话失效/页面卡住」。
func (e *Engine) EnsurePage(ctx *PageCtx, cookies map[string]string, expires map[string]int, accountID string) (Session, error) {
	if ctx.Page != nil && (accountID == "" || ctx.AccID == accountID) {
		if ok, err := ctx.Page.JS(jsHasTextarea, false, 0); err == nil && ok.Truthy() {
			if e.waitWSReady(ctx.Page, 8*time.Second) {
				return ctx.Page, nil
			}
		}
	}
	e.closeCurrentPage(ctx)

	// 仅当距离上次 HTTP 续签超过 10 分钟才在主链路打 /api/session，避免每次切号重复阻塞
	nowTS := float64(e.now().UnixNano()) / 1e9
	if accountID == "" || nowTS-e.renewAt(accountID) > 600 {
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
				e.setRenewAt(accountID, nowTS)
			}
		}
	}

	page, err := e.openPage(ctx)
	if err != nil {
		return nil, err
	}
	e.ApplyCookies(page, cookies, expires)
	if _, err := page.Send("Page.navigate", map[string]any{"url": "https://muse.ai/thread/new"}, 0); err != nil {
		return nil, err
	}
	// 先登记进任务上下文：失败路径的清理也要能关掉这个页面（上游注释同义）。
	ctx.Page = page
	e.setPage(page)
	if e.waitWSReady(page, time.Duration(e.CFG.LoginWait)*time.Second) {
		ctx.AccID = accountID
		e.setAccountID(accountID)
		return page, nil
	}

	// 区分「会话失效（被踢回登录页）」「网关一直 Connecting...」与「页面加载卡住」。
	body := ""
	if v, err := page.JS(jsBodyText, false, 0); err == nil {
		body = strings.ToLower(v.Str2())
	}
	e.closeCurrentPage(ctx)
	if strings.Contains(body, "connecting...") {
		return nil, &ErrGeneration{Msg: "页面网关连接超时 (Connecting...)，正在自动切换账号"}
	}
	if loginRe.MatchString(body) {
		return nil, &ErrAuth{Msg: "会话已被 muse.ai 登出（可能被其它登录挤掉或触发风控），" +
			"请用浏览器扩展重新导入 cookie"}
	}
	return nil, &ErrGeneration{Msg: "muse.ai 页面加载超时（未出现聊天输入框），请检查服务器网络后重试；未确认会话失效"}
}

// ensurePage 走注入点（测试用），默认就是真实的 EnsurePage。
func (e *Engine) ensurePage(ctx *PageCtx, cookies map[string]string, expires map[string]int, accountID string) (Session, error) {
	if e.EnsurePageFn != nil {
		page, err := e.EnsurePageFn(cookies, expires, accountID)
		if page != nil {
			ctx.Page = page
			e.setPage(page)
			ctx.AccID = accountID
			e.setAccountID(accountID)
		}
		return page, err
	}
	return e.EnsurePage(ctx, cookies, expires, accountID)
}

// Refresh 对应 refresh()。
func (e *Engine) Refresh(ctx *PageCtx, cookies map[string]string, expires map[string]int) (Session, error) {
	e.closeCurrentPage(ctx)
	return e.EnsurePage(ctx, cookies, expires, "")
}

// CurrentAccountID / Page / Browser 暴露内部状态（app.py 直接读这些属性）。
// 这三个是「不抢浏览器锁的读者」也会打的口子，所以读快照而不是读本体字段。
func (e *Engine) CurrentAccountID() string { return e.snap().accID }
func (e *Engine) Page() Session            { return e.snap().page }
func (e *Engine) BrowserProcUp() bool {
	p := e.snap().proc
	return p != nil && p.Alive()
}

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

// LastHTTPRenew / SetLastHTTPRenew 暴露那张「上次 HTTP 续签时间」表。
// Python 里 app.py 直接读写 engine._last_http_renew（跨模块访问私有属性），
// Go 没有这种走法，只能给两个访问器 —— 语义还是那份：同一份数据、同一个 map。
// 表本身由 renewMu 包住单次读写：保活协程在 GEN_LOCK 外也要碰它。
func (e *Engine) LastHTTPRenew(accountID string) float64 { return e.renewAt(accountID) }

func (e *Engine) SetLastHTTPRenew(accountID string, ts float64) { e.setRenewAt(accountID, ts) }
