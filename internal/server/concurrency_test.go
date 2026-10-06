package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// /admin/relogin 现在按 test_account 那样排队（见 keepalive.go 的 DEVIATION）：
// 浏览器被任务占着时它必须等，而不是把正在跑的生成半路拆掉。
// 这里用「请求先卡住、取消连接后才带着断开错误返回」证明它确实去了排队，而不是绕开锁。
func TestReloginAllQueuesBehindBrowserLock(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	acc := a.Store.AddAccount(map[string]any{"hatch_sess": "s", "hatch_gw": "g",
		"hatch_vml": "v", "hatch_native_auth_device": "d"}, "有cookie", nil)
	if acc == nil {
		t.Fatal("账号没能进池")
	}

	a.browser.ch <- struct{}{} // 假装一个生成任务正占着浏览器
	defer func() { <-a.browser.ch }()

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest("POST", "/admin/relogin", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer cap-key")
		a.ServeHTTP(rec, r)
	}()

	select {
	case <-done:
		t.Fatalf("锁被占着时 /admin/relogin 不该立刻返回，得到：%s", rec.Body.String())
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消连接后 /admin/relogin 仍没退出")
	}
	if body := rec.Body.String(); rec.Code != 200 || !strings.Contains(body, "客户端已断开") {
		t.Errorf("排队后被取消应回断开说明，得到 %d：%s", rec.Code, body)
	}
	// 全程没起过浏览器：Engine.Start 只有抢到锁才会被调到，而锁一直被占着。
	if a.Engine.BrowserProcUp() {
		t.Error("排队期间不该有任何浏览器动作")
	}
}

// 面板轮换 Key 与请求鉴权是并发发生的：auth() 每请求读一次，rotate 写一次。
// 不加锁读一个 string 在 Go 里能读到撕裂的（指针,长度）组合 —— 表现是偶发把半截旧 Key
// 当有效 Key 用。这条测试把两边同时压起来，要求每个响应都是干净的 200/401/401 壳。
func TestAuthVersusRotationIsConsistent(t *testing.T) {
	a, _ := testApp(t, "start-key", time.Unix(1700000000, 0))
	var wg sync.WaitGroup
	stop := make(chan struct{})

	hit := func(key string) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		a.ServeHTTP(rec, r)
		if rec.Code != 200 && rec.Code != 401 {
			t.Errorf("状态码跑偏：%d %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("响应体不是完整 JSON（读到撕裂值的典型表现）：%q", rec.Body.String())
			return
		}
		if rec.Code == 200 && body["object"] != "list" {
			t.Errorf("200 却没有 models 列表：%v", body)
		}
		if rec.Code == 401 && body["error"] == nil {
			t.Errorf("401 少了 OpenAI 错误壳：%v", body)
		}
	}

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					hit(a.apiKey())
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/admin/apikey/rotate", nil)
			r.Header.Set("Authorization", "Bearer "+a.apiKey())
			a.ServeHTTP(rec, r)
			if rec.Code != 200 {
				t.Errorf("轮换失败：%d %s", rec.Code, rec.Body.String())
				return
			}
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
}
