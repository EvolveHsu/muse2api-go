package engine

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestRenewSessionHTTPGolden 覆盖 8 个分支。金样本里连"假 requests 回给 engine 什么"
// 都记了下来（served 字段），Go 侧照抄回放，避免我脑补响应把断言做假。
func TestRenewSessionHTTPGolden(t *testing.T) {
	g := loadEngineGolden(t)
	sections := []struct {
		name string
		data map[string]any
	}{
		{"renew_ok", g.Renew},
		{"renew_401", g.Renew401},
		{"renew_403", g.Renew403},
		{"renew_200_notjson", g.RenewNoJSON},
		{"renew_unassigned", g.RenewPending},
		{"renew_no_wake", g.RenewNoWake},
		{"renew_vm_disabled", g.RenewVMOFF},
		{"renew_500", g.Renew500},
	}

	for _, sc := range sections {
		if sc.data == nil {
			t.Errorf("%s: 金样本里没有这一节", sc.name)
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			cfg := testCfg(t)
			hc := &fakeHTTP{respond: respondFromGolden(sc.data)}
			e := fixedEngine(t, cfg, hc)

			wake := sc.name != "renew_no_wake"
			res, err := e.RenewSessionHTTP(
				map[string]string{"hatch_sess": "s", "hatch_gw": "g"},
				map[string]int{"hatch_sess": 1800000000}, wake)

			kind, msg, hasErr := sectionError(sc.data)
			switch {
			case hasErr:
				if err == nil {
					t.Fatalf("期望 %s: %q，结果却成功了", kind, msg)
				}
				if err.Error() != msg {
					t.Errorf("错误文案不一致\n py=%q\n go=%q", msg, err.Error())
				}
				switch kind {
				case "MuseAuthError":
					if !IsAuthErr(err) {
						t.Errorf("应该是 *ErrAuth（会话失效），得到 %T", err)
					}
				case "MuseGenerationError":
					var ge *ErrGeneration
					if !errors.As(err, &ge) {
						t.Errorf("应该是 *ErrGeneration（不判 Cookie 失效），得到 %T", err)
					}
				default:
					t.Errorf("金样本里的异常类型不认识: %q", kind)
				}
			default:
				if err != nil {
					t.Fatalf("Python 侧成功，Go 却报错: %v", err)
				}
				want := decodeResult(t, sc.data)
				gotRaw, _ := json.Marshal(res)
				var got map[string]any
				if err := json.Unmarshal(gotRaw, &got); err != nil {
					t.Fatal(err)
				}
				if !jsonEquivalent(want, got) {
					t.Errorf("结果不一致\n py=%v\n go=%v", want, got)
				}
			}

			wantCalls := decodeRenewCalls(t, sc.data)
			gotCalls := hc.got()
			if len(gotCalls) != len(wantCalls) {
				t.Fatalf("调用数不同：go=%d py=%d (%v)", len(gotCalls), len(wantCalls), namesOf(gotCalls))
			}
			for i, w := range wantCalls {
				c := gotCalls[i]
				if c.Method != w.M || c.URL != w.URL {
					t.Errorf("第 %d 次调用不同: go=%s %s py=%s %s", i, c.Method, c.URL, w.M, w.URL)
				}
				if c.Timeout != time.Duration(w.Timeout*float64(time.Second)) {
					t.Errorf("第 %d 次超时不同: go=%v py=%v", i, c.Timeout, w.Timeout)
				}
				for k, v := range w.Headers {
					if k == "Cookie" {
						if cookieSet(c.Headers["Cookie"]) != cookieSet(v) {
							t.Errorf("Cookie 集合不同\n py=%q\n go=%q", v, c.Headers["Cookie"])
						}
						continue
					}
					if c.Headers[k] != v {
						t.Errorf("头 %s 不同: go=%q py=%q", k, c.Headers[k], v)
					}
				}
				if w.M == "POST" && w.JSON != nil {
					body, _ := c.Body.(map[string]any)
					for k, v := range w.JSON {
						if k == "connect_attempt_id" {
							if !uuidRe.MatchString(asString2(body[k])) {
								t.Errorf("connect_attempt_id 不是 UUID 格式: %v", body[k])
							}
							continue
						}
						if !jsonEquivalent(v, body[k]) {
							t.Errorf("body.%s 不同: go=%v py=%v", k, body[k], v)
						}
					}
				}
				if w.AllowRedirects != nil && w.M == "GET" && c.Allow != *w.AllowRedirects {
					t.Errorf("allow_redirects 不同: go=%v py=%v", c.Allow, *w.AllowRedirects)
				}
			}
		})
	}
}

// TestRenewSkipsEmptyCookieValue 单独钉住这条：Set-Cookie 值为空时不写进 cookie 池。
// 金样本里 renew_ok 的 served 就包含一条 ["hatch_empty","",0]。
func TestRenewSkipsEmptyCookieValue(t *testing.T) {
	g := loadEngineGolden(t)
	cfg := testCfg(t)
	served := sectionServed(g.Renew)
	hasEmpty := false
	for _, c := range served.Cookies {
		if n, _ := c[0].(string); n == "hatch_empty" {
			hasEmpty = true
		}
	}
	if !hasEmpty {
		t.Skip("金样本 served 里没有空值 cookie 样本，跳过")
	}
	hc := &fakeHTTP{respond: respondFromGolden(g.Renew)}
	e := fixedEngine(t, cfg, hc)
	res, err := e.RenewSessionHTTP(map[string]string{"hatch_sess": "s"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := res.Cookies["hatch_empty"]; ok {
		t.Errorf("空值 cookie 不该被并入，得到 %q", v)
	}
	if _, ok := res.CookiesExp["hatch_empty"]; ok {
		t.Error("空值 cookie 的 expires 也不该被并入")
	}
	if res.Cookies["hatch_vml"] != "NEWVML" || res.CookiesExp["hatch_vml"] != 1700086400 {
		t.Errorf("hatch_vml 应被并入新值: %v / %v", res.Cookies["hatch_vml"], res.CookiesExp["hatch_vml"])
	}
}

// respondFromGolden 按金样本记录的 served 字段回放假 HTTP 应答。
func respondFromGolden(section map[string]any) func(httpCall) (Response, error) {
	sv := sectionServed(section)
	var body []byte
	switch {
	case sv.RaiseJSON:
		body = []byte("oops") // 让 Unmarshal 失败，对应 Python 的 ValueError 分支
	case sv.Payload == nil:
		body, _ = json.Marshal(map[string]any{
			"status": "assigned", "vm_id": "vm-1", "vm_state": "SLEEPING"})
	default:
		body, _ = json.Marshal(sv.Payload)
	}
	var cookies []RespCookie
	for _, c := range sv.Cookies {
		name, _ := c[0].(string)
		value, _ := c[1].(string)
		exp, _ := c[2].(float64)
		cookies = append(cookies, RespCookie{Name: name, Value: value, Expires: int64(exp)})
	}
	return func(c httpCall) (Response, error) {
		if c.URL == sessionURL {
			return Response{StatusCode: sv.Status, Body: body, Cookies: cookies}, nil
		}
		return Response{StatusCode: 200, Body: []byte(`{}`)}, nil
	}
}

func namesOf(calls []httpCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method+" "+c.URL)
	}
	return out
}
