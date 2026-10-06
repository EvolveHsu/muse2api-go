package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"muse2api-go/internal/engine"
)

// keepalive_test.go 验的是保活那条链路上最容易写错的部分：
// 「什么情况下才允许把账号判成失效」。这一段全走假 HTTP，不碰浏览器、不花额度。

// stubHTTP 实现 engine.HTTPClient，按 URL 返回预设响应。
type stubHTTP struct {
	calls   []string
	session func() engine.Response
	wake    func() engine.Response
}

func (s *stubHTTP) Get(url string, _ map[string]string, _ time.Duration, _ bool) (engine.Response, error) {
	s.calls = append(s.calls, "GET "+url)
	if strings.Contains(url, "/api/session") && s.session != nil {
		return s.session(), nil
	}
	return engine.Response{StatusCode: 599, Body: []byte("{}")}, nil
}

func (s *stubHTTP) Post(url string, _ map[string]string, _ any, _ time.Duration) (engine.Response, error) {
	s.calls = append(s.calls, "POST "+url)
	if s.wake != nil {
		return s.wake(), nil
	}
	return engine.Response{StatusCode: 200, Body: []byte("{}")}, nil
}

func (s *stubHTTP) Put(url string, _ map[string]string, _ time.Duration) (engine.Response, error) {
	s.calls = append(s.calls, "PUT "+url)
	return engine.Response{StatusCode: 200, Body: []byte("{}")}, nil
}

func assignedSession() engine.Response {
	return engine.Response{
		StatusCode: 200,
		Body:       []byte(`{"status":"assigned","vm_id":"vm-1","vm_state":"RUNNING"}`),
		Cookies:    []engine.RespCookie{{Name: "hatch_vml", Value: "新值", Expires: 1800000000}},
	}
}

func seededApp(t *testing.T, hc engine.HTTPClient) (*App, string) {
	t.Helper()
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0), hc)
	acc := a.Store.AddAccount(map[string]any{"hatch_sess": "旧值", "hatch_vml": "旧令牌"},
		"标签A", nil)
	a.Store.UpdateAccount(acc["id"].(string), map[string]any{"ok": true, "note": "上次确认"})
	return a, acc["id"].(string)
}

func probe(t *testing.T, a *App, aid string, checkQuota bool) map[string]any {
	t.Helper()
	got := a.probeAccountSync(aid, checkQuota)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func accountOf(a *App, aid string) map[string]any {
	return a.Store.GetAccount(aid)
}

// 只有 401 才把账号判成失效；403/5xx/非 JSON/未 assigned 一律只记诊断。
func TestProbeAccountKeepsConfirmedState(t *testing.T) {
	cases := []struct {
		name      string
		session   func() engine.Response
		wantOK    bool
		wantNote  string
		wantKept  bool // 账号的 ok 字段是否必须保持 true
		wantErrIn string
	}{
		{name: "200 assigned", session: assignedSession, wantOK: true,
			wantNote: "会话有效 · 自动保活 (VM: RUNNING)"},
		{name: "401 判失效", session: func() engine.Response {
			return engine.Response{StatusCode: 401, Body: []byte("{}")}
		}, wantOK: false, wantNote: "保活认证失败", wantErrIn: "401"},
		{name: "403 不判失效", session: func() engine.Response {
			return engine.Response{StatusCode: 403, Body: []byte("{}")}
		}, wantOK: false, wantNote: "保活未确认（保留账号状态）", wantKept: true, wantErrIn: "403"},
		{name: "500 不判失效", session: func() engine.Response {
			return engine.Response{StatusCode: 500, Body: []byte("{}")}
		}, wantOK: false, wantNote: "保活未确认（保留账号状态）", wantKept: true, wantErrIn: "500"},
		{name: "非 JSON 不判失效", session: func() engine.Response {
			return engine.Response{StatusCode: 200, Body: []byte("<html>")}
		}, wantOK: false, wantNote: "保活未确认（保留账号状态）", wantKept: true, wantErrIn: "非 JSON"},
		{name: "未 assigned 不判失效", session: func() engine.Response {
			return engine.Response{StatusCode: 200, Body: []byte(`{"status":"unassigned"}`)}
		}, wantOK: false, wantNote: "保活未确认（保留账号状态）", wantKept: true, wantErrIn: "assigned"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := &stubHTTP{session: c.session}
			a, aid := seededApp(t, hc)
			got := probe(t, a, aid, false)
			if ok, _ := got["ok"].(bool); ok != c.wantOK {
				t.Errorf("返回的 ok 不同：%v，期望 %v", got["ok"], c.wantOK)
			}
			if c.wantErrIn != "" && !strings.Contains(strOf(got["error"]), c.wantErrIn) {
				t.Errorf("错误信息该含 %q，得到 %v", c.wantErrIn, got["error"])
			}
			acc := accountOf(a, aid)
			note := strOf(acc["note"])
			if !strings.HasPrefix(note, c.wantNote) {
				t.Errorf("账号 note 不同：%q，期望前缀 %q", note, c.wantNote)
			}
			if c.wantKept && acc["ok"] != true {
				t.Errorf("非 401 失败不该覆盖上次已确认的状态：ok=%v", acc["ok"])
			}
			if c.name == "401 判失效" && acc["ok"] != false {
				t.Errorf("401 应当把账号判为失效，ok=%v", acc["ok"])
			}
			if c.name == "200 assigned" {
				cookies, _ := acc["cookies"].(map[string]any)
				if strOf(cookies["hatch_vml"]) != "新值" {
					t.Errorf("续签回来的 cookie 没写回：%v", cookies)
				}
				if int64(numOr(acc["expires_at"])) != 1800000000 {
					t.Errorf("expires_at 该按新的 hatch_vml 重算：%v", acc["expires_at"])
				}
			}
		})
	}
}

// check_quota=true 时只允许抢锁（non-blocking），抢不到就跳过额度，不能排队。
func TestProbeAccountSkipsQuotaWhenBusy(t *testing.T) {
	hc := &stubHTTP{session: assignedSession}
	a, aid := seededApp(t, hc)
	// 先把锁占住，模拟生成正在进行。
	hold := make(chan struct{})
	go func() {
		_ = a.browser.runBlocking(hold, func() { <-hold })
	}()
	time.Sleep(20 * time.Millisecond)
	got := probe(t, a, aid, true)
	close(hold)
	if ok, _ := got["ok"].(bool); !ok {
		t.Fatalf("保活本身该成功：%v", got)
	}
	if got["quota"] != nil {
		t.Errorf("锁被占用时该跳过额度读取，实际拿到 %v", got["quota"])
	}
}

func TestRunKeepaliveAll(t *testing.T) {
	t.Run("会话充足则跳过", func(t *testing.T) {
		hc := &stubHTTP{session: assignedSession}
		a, aid := seededApp(t, hc)
		a.Store.UpdateAccount(aid, map[string]any{
			"expires_at": a.now().Unix() + 10*24*3600, "last_keepalive": a.now().Unix(),
			"ok": true})
		got := a.runKeepaliveAll(false)
		b, _ := json.Marshal(got)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if intOf(m["refreshed_count"]) != 0 || intOf(m["skipped_count"]) != 1 {
			t.Errorf("该整体跳过：%s", b)
		}
		if strings.Contains(string(b), "会话充足且近期已保活") == false {
			t.Errorf("跳过原因该写进结果：%s", b)
		}
		a.kaMu.Lock()
		next := a.kaState.NextRun
		a.kaMu.Unlock()
		if next == nil {
			t.Errorf("跑完一轮该记下 next_run")
		}
	})
	t.Run("force 则全部续签", func(t *testing.T) {
		hc := &stubHTTP{session: assignedSession}
		a, _ := seededApp(t, hc)
		summary := a.runKeepaliveAll(true)
		if got := intOf(objOf(t, summary)["refreshed_count"]); got != 1 {
			t.Errorf("force 应刷新 1 个账号，得到 %d", got)
		}
	})
	t.Run("执行中直接 busy", func(t *testing.T) {
		a, _ := seededApp(t, &stubHTTP{})
		a.kaMu.Lock()
		a.kaState.Running = true
		a.kaMu.Unlock()
		rec := httptest.NewRecorder()
		reqKA := httptest.NewRequest("POST", "/admin/accounts/keepalive", nil)
		reqKA.Header.Set("Authorization", "Bearer cap-key")
		a.ServeHTTP(rec, reqKA)
		if !strings.Contains(rec.Body.String(), `"status":"busy"`) {
			t.Errorf("忙时应回 busy：%s", rec.Body.String())
		}
	})
}

func objOf(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 这几条分支不需要浏览器就能验完：账号不存在 / 没有 cookie / 池子里没账号。
// 一旦走到 Engine.Start() 就会去拉 Chromium，不属于单测范围（Stage E 双跑再验）。
func TestBrowserAdminPreconditions(t *testing.T) {
	bare, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	bareID := strOf(bare.Store.AddAccount(map[string]any{}, "没cookie", nil)["id"])

	cases := []struct {
		app          *App
		method, path string
		want         int
		wantContains string
	}{
		{bare, "POST", "/admin/accounts/nope/test", 404, "账号不存在"},
		{bare, "POST", "/admin/accounts/nope/relogin", 404, "账号不存在"},
		{bare, "POST", "/admin/accounts/nope/quota", 404, "账号不存在"},
		{bare, "POST", "/admin/accounts/" + bareID + "/test", 400, "该账号没有 cookie"},
		{bare, "POST", "/admin/accounts/" + bareID + "/quota", 400, "该账号没有 cookie"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Authorization", "Bearer cap-key")
		c.app.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s %s 状态码 go=%d want=%d（%s）", c.method, c.path, rec.Code, c.want, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.wantContains) {
			t.Errorf("%s 响应该含 %q，得到 %s", c.path, c.wantContains, rec.Body.String())
		}
	}

	// 空池子：两条「先挑账号」的路由都在起浏览器之前就拒掉
	for _, path := range []string{"/admin/relogin", "/admin/quota"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("Authorization", "Bearer cap-key")
		noop, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
		noop.ServeHTTP(rec, r)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "没有可用账号") {
			t.Errorf("%s 该回 400 没有可用账号，得到 %d：%s", path, rec.Code, rec.Body.String())
		}
	}

	// 单账号保活：cookie 不齐全时直接回诊断，不碰浏览器
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/admin/accounts/"+bareID+"/keepalive", nil)
	r2.Header.Set("Authorization", "Bearer cap-key")
	bare.ServeHTTP(rec2, r2)
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), "账号无有效 cookie") {
		t.Errorf("无 cookie 的账号保活该回诊断，得到 %d：%s", rec2.Code, rec2.Body.String())
	}
}
