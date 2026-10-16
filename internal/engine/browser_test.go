package engine

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"muse2api-go/internal/config"
)

var (
	fixedNow  = 1700000000.5
	fixedTime = time.Unix(1700000000, 500000000)
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	base := t.TempDir()
	return &config.Config{
		BaseDir:     base,
		Host:        "0.0.0.0",
		Port:        18610,
		Chromium:    "/usr/bin/chromium",
		CdpPort:     19210,
		HomeDir:     base,
		ExtraPath:   "/snap/bin",
		SiteURL:     "https://muse.ai/",
		LoginWait:   60,
		ProfileRoot: filepath.Join(base, "profiles"),
	}
}

func fixedEngine(t *testing.T, cfg *config.Config, hc *fakeHTTP) *Engine {
	e := NewEngine(cfg, hc, func(wsURL string, timeout time.Duration) (Session, error) {
		return &fakeSession{}, nil
	})
	e.now = func() time.Time { return fixedTime }
	e.sleep = func(time.Duration) {}
	return e
}

// TestChromiumArgsMatchGolden 逐条比对 argv —— 少一个 flag 或顺序变了都会被抓到。
func TestChromiumArgsMatchGolden(t *testing.T) {
	g := loadEngineGolden(t)
	cfg := testCfg(t)
	e := fixedEngine(t, cfg, &fakeHTTP{})

	got := e.chromiumArgs()
	want := g.Argv.Argv
	if len(got) != len(want) {
		t.Fatalf("参数个数不同：go=%d python=%d\ngo=%v\npy=%v", len(got), len(want), got, want)
	}
	// user-data-dir 跟本机临时目录走，只校验形式一致。
	for i := range want {
		if strings.HasPrefix(want[i], "--user-data-dir=") {
			if !strings.HasPrefix(got[i], "--user-data-dir=") {
				t.Fatalf("第 %d 项应是 user-data-dir，得到 %q", i, got[i])
			}
			if got[i] != "--user-data-dir="+e.profileDir() {
				t.Errorf("user-data-dir 没指向 cfg.ProfileDir(): %q", got[i])
			}
			want[i] = got[i]
		}
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("第 %d 项不同:\n go=%q\n py=%q", i, got[i], want[i])
			}
		}
		t.FailNow()
	}
	t.Logf("argv %d 项与真 Python 一致（含 --window-size=1440,2400 与 about:blank 收尾）", len(got))
}

// TestApplyCookiesMatchGolden 校验 cookie 注入的整串 CDP 调用，含两个坑：
// 空值 cookie 要跳过、过去/太近的 expires 要钳到 now+7d。
func TestApplyCookiesMatchGolden(t *testing.T) {
	g := loadEngineGolden(t)
	cfg := testCfg(t)
	e := fixedEngine(t, cfg, &fakeHTTP{})

	page := &fakeSession{}
	e.ApplyCookies(page,
		map[string]string{"hatch_sess": "s", "hatch_gw": "", "hatch_vml": "v",
			"hatch_native_auth_device": "d", "extra": "e"},
		map[string]int{"hatch_sess": 1699000000, "hatch_vml": 1700003600,
			"hatch_gw": 1800000000, "bad": 0})

	got := page.sendsAsPairs()
	if len(got) != len(g.ApplyCookies) {
		t.Fatalf("调用数不同：go=%d python=%d", len(got), len(g.ApplyCookies))
	}
	// Go 侧按名字排序遍历，Python 按插入顺序；把两边都规约成 "name|domain" 索引再逐条比。
	index := func(rows [][]any) map[string]any {
		out := map[string]any{}
		for _, r := range rows {
			method, _ := r[0].(string)
			p, _ := r[1].(map[string]any)
			if p == nil {
				out[method+"/-"] = nil
				continue
			}
			key := method + "/" + asString2(p["name"]) + "@" + asString2(p["domain"])
			out[key] = p
		}
		return out
	}
	wantIdx := index(g.ApplyCookies)
	gotIdx := map[string]any{}
	for _, s := range page.sends {
		if s.Params == nil {
			gotIdx[s.Method+"/-"] = nil
			continue
		}
		gotIdx[s.Method+"/"+asString2(s.Params["name"])+"@"+asString2(s.Params["domain"])] = map[string]any(s.Params)
	}
	if len(wantIdx) != len(gotIdx) {
		t.Fatalf("索引规模不同 %d vs %d", len(wantIdx), len(gotIdx))
	}
	for k, w := range wantIdx {
		g2, ok := gotIdx[k]
		if !ok {
			t.Errorf("缺少调用 %s", k)
			continue
		}
		if !jsonEquivalent(w, g2) {
			t.Errorf("%s 参数不一致\n py=%v\n go=%v", k, w, g2)
		}
	}
	// 明确断言那两个坑，避免"两边一起错"。
	if _, ok := gotIdx["Network.setCookie/hatch_gw@.muse.ai"]; ok {
		t.Error("空值 cookie 不应被注入")
	}
	if p, _ := gotIdx["Network.setCookie/hatch_sess@.muse.ai"].(map[string]any); p != nil {
		if p["expires"] != fixedNow+7*86400 {
			t.Errorf("过期的 hatch_sess 应被钳到 now+7d，得到 %v", p["expires"])
		}
	}
}

func asString2(v any) string {
	s, _ := v.(string)
	return s
}

// TestOpenPageMatchesGolden 校验开页的三次 HTTP（含超时值）与四个 enable。
func TestOpenPageMatchesGolden(t *testing.T) {
	g := loadEngineGolden(t)
	cfg := testCfg(t)

	hc := &fakeHTTP{}
	var made *fakeSession
	e := NewEngine(cfg, hc, func(wsURL string, timeout time.Duration) (Session, error) {
		made = &fakeSession{}
		return made, nil
	})
	e.now = func() time.Time { return fixedTime }
	e.sleep = func(time.Duration) {}
	hc.respond = func(c httpCall) (Response, error) {
		switch {
		case strings.HasSuffix(c.URL, "/json/list"):
			return Response{StatusCode: 200, Body: []byte(
				`[{"id":"T1","type":"page","url":"https://muse.ai/thread/new","webSocketDebuggerUrl":"ws://x"},
				  {"id":"T2","type":"other","url":"x"}]`)}, nil
		case strings.Contains(c.URL, "/json/close/"):
			return Response{StatusCode: 200, Body: []byte(`"Target closed"`)}, nil
		default:
			return Response{StatusCode: 200, Body: []byte(
				`{"id":"T3","webSocketDebuggerUrl":"ws://127.0.0.1:19210/devtools/page/T3"}`)}, nil
		}
	}

	// 这个场景没有 browser 级会话（e.browser == nil），走的是上游的"关掉旧页面再开"回退路径；
	// 1.6.0 新增的 Incognito 隔离路径由 TestOpenPageIsolatedContext 单独覆盖。
	if _, err := e.openPage(e.NewPageCtx()); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(g.OpenPage["calls"])
	var wantCalls []renewCall
	if err := json.Unmarshal(raw, &wantCalls); err != nil {
		t.Fatal(err)
	}
	gotCalls := hc.got()
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("HTTP 调用数不同：go=%d py=%d", len(gotCalls), len(wantCalls))
	}
	for i, w := range wantCalls {
		gc := gotCalls[i]
		if gc.Method != w.M || gc.URL != w.URL {
			t.Errorf("第 %d 次调用不同: go=%s %s py=%s %s", i, gc.Method, gc.URL, w.M, w.URL)
		}
		if gc.Timeout != time.Duration(w.Timeout*float64(time.Second)) {
			t.Errorf("第 %d 次超时不同: go=%v py=%v", i, gc.Timeout, w.Timeout)
		}
	}

	rawE, _ := json.Marshal(g.OpenPage["enables"])
	var enables [][]any
	if err := json.Unmarshal(rawE, &enables); err != nil {
		t.Fatal(err)
	}
	gotEn := made.sends
	if len(gotEn) != len(enables) {
		t.Fatalf("enable 调用数不同：go=%d py=%d", len(gotEn), len(enables))
	}
	for i := range enables {
		w := enables[i]
		g := gotEn[i]
		var gp any
		if g.Params != nil {
			gp = g.Params
		}
		// downloadPath 跟本机临时目录走，只校验"确实指向 cfg.DownloadDir()"。
		if wm, ok := w[1].(map[string]any); ok {
			if dp, has := wm["downloadPath"]; has {
				gpm, _ := gp.(map[string]any)
				if gpm["downloadPath"] != cfg.DownloadDir() {
					t.Errorf("第 %d 个 enable 的 downloadPath 不是 cfg.DownloadDir(): %v", i, gpm["downloadPath"])
				}
				wm["downloadPath"] = gpm["downloadPath"]
				_ = dp
			}
		}
		if !jsonEquivalent([]any{w[0], w[1]}, []any{g.Method, gp}) {
			t.Errorf("第 %d 个 enable 不一致\n py=%v\n go=%v %v", i, w, g.Method, g.Params)
		}
	}
}

// TestOpenPageIsolatedContextMatchesGolden 校验 1.6.0 新增的隔离上下文开页路径：
// 浏览器级连接上恰好两条 Target.* 调用、页面 CDP 四个 enable、上下文登记到 active 集合。
func TestOpenPageIsolatedContextMatchesGolden(t *testing.T) {
	g := loadEngineGolden(t)
	sec := g.OpenPageIsolated
	cfg := testCfg(t)

	browser := &fakeSession{}
	browser.sendRet = func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "Target.createBrowserContext":
			return map[string]any{"result": map[string]any{"browserContextId": "CTX-1"}}, nil
		case "Target.createTarget":
			return map[string]any{"result": map[string]any{"targetId": "T7"}}, nil
		}
		return map[string]any{"result": map[string]any{}}, nil
	}
	var made *fakeSession
	var madeWS string
	e := NewEngine(cfg, &fakeHTTP{}, func(wsURL string, timeout time.Duration) (Session, error) {
		madeWS = wsURL
		made = &fakeSession{}
		return made, nil
	})
	e.now = func() time.Time { return fixedTime }
	e.sleep = func(time.Duration) {}
	e.setBrowser(browser)

	ctx := e.NewPageCtx()
	if _, err := e.openPage(ctx); err != nil {
		t.Fatal(err)
	}

	gb, _ := json.Marshal(browser.sendsAsPairs())
	var gotBrowser any
	_ = json.Unmarshal(gb, &gotBrowser)
	if !jsonEquivalent(sec["browser"], gotBrowser) {
		t.Errorf("浏览器级调用不同:\n py=%v\n go=%v", sec["browser"], gotBrowser)
	}

	ge, _ := json.Marshal(made.sendsAsPairs())
	var gotEnables []any
	_ = json.Unmarshal(ge, &gotEnables)
	// downloadPath 跟本机临时目录走：把金样本里的那一条换成 Go 的实际值，只校验其它字段。
	rawWant, _ := json.Marshal(sec["enables"])
	var wantEnables []any
	_ = json.Unmarshal(rawWant, &wantEnables)
	for i, raw := range wantEnables {
		pair, _ := raw.([]any)
		if len(pair) != 2 {
			continue
		}
		pm, _ := pair[1].(map[string]any)
		if pm == nil {
			continue
		}
		if _, has := pm["downloadPath"]; !has {
			continue
		}
		gotPair, _ := gotEnables[i].([]any)
		if len(gotPair) == 2 {
			if gpm, ok := gotPair[1].(map[string]any); ok {
				pm["downloadPath"] = gpm["downloadPath"]
			}
		}
	}
	if !jsonEquivalent(wantEnables, gotEnables) {
		t.Errorf("页面 enable 不同:\n py=%v\n go=%v", wantEnables, gotEnables)
	}
	if madeWS != asString2(sec["ws_url"]) {
		t.Errorf("页面 CDP 的 ws URL 不同: go=%q py=%v", madeWS, sec["ws_url"])
	}
	if ctx.ContextID != asString2(sec["context_id"]) {
		t.Errorf("ctx.ContextID 不同: go=%q py=%v", ctx.ContextID, sec["context_id"])
	}
	e.contextsMu.Lock()
	active := make([]string, 0, len(e.activeContexts))
	for id := range e.activeContexts {
		active = append(active, id)
	}
	e.contextsMu.Unlock()
	sort.Strings(active)
	var wantActive []any
	rawA, _ := json.Marshal(sec["active_contexts"])
	_ = json.Unmarshal(rawA, &wantActive)
	gotActive := make([]any, 0, len(active))
	for _, id := range active {
		gotActive = append(gotActive, id)
	}
	if !jsonEquivalent(wantActive, gotActive) {
		t.Errorf("active_contexts 不同: go=%v py=%v", gotActive, wantActive)
	}
}
