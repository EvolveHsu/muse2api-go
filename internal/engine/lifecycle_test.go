package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type launchRecord struct {
	args    []string
	env     []string
	dir     string
	logName string
	calls   int
}

// startFixture 组一个"假 launcher + 可控 /json/version"的引擎。
func startFixture(t *testing.T, versionFailures int) (*Engine, *launchRecord, *fakeHTTP) {
	t.Helper()
	cfg := testCfg(t)
	rec := &launchRecord{}
	hc := &fakeHTTP{}
	var mu sync.Mutex
	attempt := 0
	hc.respond = func(c httpCall) (Response, error) {
		if strings.HasSuffix(c.URL, "/json/version") {
			mu.Lock()
			attempt++
			n := attempt
			mu.Unlock()
			if n <= versionFailures {
				return Response{}, &ReqError{Kind: "ConnectionError", Err: os.ErrDeadlineExceeded}
			}
			return Response{StatusCode: 200,
				Body: []byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:19210/devtools/page/BROWSER"}`)}, nil
		}
		return Response{StatusCode: 599, Body: []byte(`{}`)}, nil
	}
	e := NewEngine(cfg, hc, func(wsURL string, timeout time.Duration) (Session, error) {
		return &fakeSession{}, nil
	})
	e.now = func() time.Time { return fixedTime }
	e.sleep = func(time.Duration) {}
	e.waitTimeout = 50 * time.Millisecond
	e.Launch = func(args, env []string, dir string, lf *os.File) (Process, error) {
		rec.calls++
		rec.args = args
		rec.env = env
		rec.dir = dir
		rec.logName = lf.Name()
		return &fakeProc{alive: true, honorsTerm: true}, nil
	}
	// Start() 会一直保持 chromium.log 的句柄（Python 的 self._log 也如此），
	// 不 Stop 的话 Windows 下 t.TempDir 清理不掉。
	t.Cleanup(e.Stop)
	return e, rec, hc
}

func TestStartLaunchesWithGoldenArgv(t *testing.T) {
	g := loadEngineGolden(t)
	e, rec, _ := startFixture(t, 1) // 第一次 /json/version 失败 -> 走真正的启动分支

	if err := e.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("应恰好启动一次进程，得到 %d", rec.calls)
	}
	if strings.Join(rec.args, "\x00") != strings.Join(e.chromiumArgs(), "\x00") {
		t.Errorf("launcher 收到的 argv 与 chromiumArgs() 不一致:\n %v\n %v", rec.args, e.chromiumArgs())
	}
	if len(rec.args) != len(g.Argv.Argv) {
		t.Fatalf("argv 条数与 Python 不同: go=%d py=%d", len(rec.args), len(g.Argv.Argv))
	}
	if !strings.HasPrefix(rec.logName, e.CFG.DataDir()) ||
		!strings.HasSuffix(rec.logName, "chromium.log") {
		t.Errorf("日志句柄不在 data_dir/chromium.log: %s", rec.logName)
	}
	if !e.alive() {
		t.Error("Start 之后应认为进程存活")
	}
	// 再调一次不应重复启动（对应 proc.poll() is None and self.browser）
	e2 := e
	if err := e2.Start(); err != nil || rec.calls != 1 {
		t.Errorf("Start 幂等性坏了: calls=%d err=%v", rec.calls, err)
	}
}

// TestStartReusesExistingCDP 对应 Python 的"尝试复用已有健康 CDP"：不启动新进程。
func TestStartReusesExistingCDP(t *testing.T) {
	e, rec, _ := startFixture(t, 0) // /json/version 立刻成功
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 0 {
		t.Errorf("复用成功时不该启动进程，启动了 %d 次", rec.calls)
	}
	if e.browser == nil {
		t.Error("复用路径也应建立 browser 会话")
	}
}

func TestStartFailsAfter90Retries(t *testing.T) {
	e, rec, _ := startFixture(t, 1000) // 永远连不上
	var sleeps int
	e.sleep = func(time.Duration) { sleeps++ }

	err := e.Start()
	if err == nil {
		t.Fatal("应当失败")
	}
	if !strings.HasPrefix(err.Error(), "Chromium 启动失败: ") {
		t.Errorf("错误文案变了: %q", err.Error())
	}
	var ge *ErrGeneration
	if !asGeneration(err, &ge) {
		t.Errorf("应是 *ErrGeneration，得到 %T", err)
	}
	if sleeps != 90 {
		t.Errorf("重试次数应为 90，得到 %d", sleeps)
	}
	if rec.calls != 1 {
		t.Errorf("进程应启动一次，得到 %d", rec.calls)
	}
}

func asGeneration(err error, target **ErrGeneration) bool {
	g, ok := err.(*ErrGeneration)
	if ok {
		*target = g
	}
	return ok
}

// TestStartCleansSingletonLocks 校验残留锁清理，包括一个指向不存在目标的软链接。
func TestStartCleansSingletonLocks(t *testing.T) {
	e, _, _ := startFixture(t, 1)
	prof := e.profileDir()
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if err := os.WriteFile(filepath.Join(prof, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 再造一个悬空软链接：Python 用 exists() or islink() 判断，Lstat 覆盖同样情形。
	dangling := filepath.Join(prof, "dangling")
	if err := os.Symlink(filepath.Join(prof, "nowhere"), dangling); err == nil {
		defer os.Remove(dangling) // 删不掉也无妨，Windows 无权限时上面会返回错误
	}

	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if _, err := os.Lstat(filepath.Join(prof, name)); !os.IsNotExist(err) {
			t.Errorf("残留锁没被清掉: %s", name)
		}
	}
}

func TestStopTerminatesThenKillsWhenStuck(t *testing.T) {
	e, _, _ := startFixture(t, 1)
	p := &fakeProc{alive: true, honorsTerm: false} // 不理 SIGTERM
	e.proc = p
	e.browser = &fakeSession{}
	e.page = &fakeSession{}
	e.waitTimeout = 20 * time.Millisecond
	e.sleep = func(time.Duration) {}

	e.Stop()

	term, kills := p.counts()
	if term != 1 {
		t.Errorf("应调用一次 Terminate，得到 %d", term)
	}
	if kills != 1 {
		t.Errorf("超时后应调用一次 Kill，得到 %d", kills)
	}
	if e.proc != nil || e.browser != nil || e.page != nil {
		t.Error("Stop 后应把 proc/browser/page 都清空")
	}
}

func TestStopDoesNotKillWhenTerminateWorks(t *testing.T) {
	e, _, _ := startFixture(t, 1)
	p := &fakeProc{alive: true, honorsTerm: true}
	e.proc = p
	e.waitTimeout = time.Second
	e.sleep = func(time.Duration) {}

	e.Stop()

	if _, kills := p.counts(); kills != 0 {
		t.Errorf("SIGTERM 已生效还去 KILL，kills=%d", kills)
	}
}

// TestStopClosesPageBeforeBrowser 对应 Python 的 for c in (self.page, self.browser)。
func TestStopClosesPageBeforeBrowser(t *testing.T) {
	e, _, _ := startFixture(t, 1)
	order := []string{}
	e.page = &orderSession{tag: "page", sink: &order}
	e.browser = &orderSession{tag: "browser", sink: &order}
	e.proc = &fakeProc{alive: false}

	e.Stop()

	if strings.Join(order, ",") != "page,browser" {
		t.Errorf("关闭顺序应为 page 再 browser，得到 %v", order)
	}
}

type orderSession struct {
	tag  string
	sink *[]string
}

func (s *orderSession) Send(string, map[string]any, time.Duration) (map[string]any, error) {
	return map[string]any{}, nil
}
func (s *orderSession) JS(string, bool, time.Duration) (Value, error) { return Null(), nil }
func (s *orderSession) Close()                                        { *s.sink = append(*s.sink, s.tag) }

func TestChildEnvPrependsExtraPathAndDefaultsHome(t *testing.T) {
	cfg := testCfg(t)
	cfg.ExtraPath = "/snap/bin"
	e := fixedEngine(t, cfg, &fakeHTTP{})

	env := e.childEnv()
	var path, home string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH":
			path = v
		case "HOME":
			home = v
		}
	}
	if !strings.HasPrefix(path, "/snap/bin"+string(os.PathListSeparator)) {
		t.Errorf("PATH 没有前置 extra_path: %q", path)
	}
	if home == "" {
		t.Error("HOME 应有值（setdefault 语义）")
	}

	// ExtraPath 为空时不该动 PATH
	cfg2 := testCfg(t)
	cfg2.ExtraPath = ""
	e2 := fixedEngine(t, cfg2, &fakeHTTP{})
	for _, kv := range e2.childEnv() {
		if k, v, _ := strings.Cut(kv, "="); k == "PATH" && strings.HasPrefix(v, string(os.PathListSeparator)) {
			t.Errorf("extra_path 为空却改坏了 PATH: %q", v)
		}
	}
}

func TestChildCWDOnlyWhenDirectoryExists(t *testing.T) {
	cfg := testCfg(t)
	e := fixedEngine(t, cfg, &fakeHTTP{})
	if e.childCWD() != cfg.HomeDir {
		t.Errorf("home_dir 是存在的目录时应设 cwd: %q", e.childCWD())
	}
	cfg.HomeDir = filepath.Join(cfg.BaseDir, "no-such-dir")
	if got := e.childCWD(); got != "" {
		t.Errorf("home_dir 不存在时不该设 cwd，得到 %q", got)
	}
}

func TestURLsFollowCdpPort(t *testing.T) {
	cfg := testCfg(t)
	cfg.CdpPort = 19210
	e := fixedEngine(t, cfg, &fakeHTTP{})
	if e.debugURL() != "http://127.0.0.1:19210/json/version" {
		t.Errorf("debugURL: %s", e.debugURL())
	}
	if e.listURL() != "http://127.0.0.1:19210/json/list" {
		t.Errorf("listURL: %s", e.listURL())
	}
}
