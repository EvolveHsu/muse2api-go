package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 假 Session

type sendCall struct {
	Method string
	Params map[string]any
}

type fakeSession struct {
	mu     sync.Mutex
	sends  []sendCall
	jsSeen []string
	closed int

	jsRet func(expr string) (Value, error)
	// sendRet 给需要按方法返回不同结果的用例用（如 Target.createBrowserContext/Target.createTarget）。
	sendRet func(method string, params map[string]any) (map[string]any, error)
}

func (f *fakeSession) Send(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	f.mu.Lock()
	f.sends = append(f.sends, sendCall{method, params})
	fn := f.sendRet
	f.mu.Unlock()
	if fn != nil {
		return fn(method, params)
	}
	return map[string]any{"result": map[string]any{}}, nil
}

func (f *fakeSession) JS(expr string, awaitPromise bool, timeout time.Duration) (Value, error) {
	f.mu.Lock()
	f.jsSeen = append(f.jsSeen, expr)
	fn := f.jsRet
	f.mu.Unlock()
	if fn != nil {
		return fn(expr)
	}
	return Null(), nil
}

func (f *fakeSession) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

// sendsAsPairs 把记录转成 [[method, params], ...]，与金样本的形态对齐。
func (f *fakeSession) sendsAsPairs() [][2]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][2]any, 0, len(f.sends))
	for _, s := range f.sends {
		var p any
		if s.Params != nil {
			p = s.Params
		}
		out = append(out, [2]any{s.Method, p})
	}
	return out
}

// ---------------------------------------------------------------- 假 HTTP

type httpCall struct {
	Method  string
	URL     string
	Timeout time.Duration
	Headers map[string]string
	Body    any
	Allow   bool
}

type fakeHTTP struct {
	mu      sync.Mutex
	calls   []httpCall
	respond func(c httpCall) (Response, error)
}

func (f *fakeHTTP) record(c httpCall) (Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	fn := f.respond
	f.mu.Unlock()
	if fn == nil {
		return Response{StatusCode: 599, Body: []byte("{}")}, nil
	}
	return fn(c)
}

func (f *fakeHTTP) Get(url string, headers map[string]string, timeout time.Duration, allow bool) (Response, error) {
	return f.record(httpCall{http.MethodGet, url, timeout, headers, nil, allow})
}

func (f *fakeHTTP) Put(url string, headers map[string]string, timeout time.Duration) (Response, error) {
	return f.record(httpCall{http.MethodPut, url, timeout, headers, nil, true})
}

func (f *fakeHTTP) Post(url string, headers map[string]string, body any, timeout time.Duration) (Response, error) {
	return f.record(httpCall{http.MethodPost, url, timeout, headers, body, true})
}

func (f *fakeHTTP) got() []httpCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]httpCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// ---------------------------------------------------------------- 假进程

type fakeProc struct {
	mu         sync.Mutex
	alive      bool
	terminates int
	kills      int
	honorsTerm bool
}

func (p *fakeProc) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive
}

func (p *fakeProc) Terminate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminates++
	if p.honorsTerm {
		p.alive = false
	}
}

func (p *fakeProc) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kills++
	p.alive = false
}

func (p *fakeProc) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminates, p.kills
}

// ---------------------------------------------------------------- 金样本

// served 是金样本里"假 requests 实际回给 engine 的东西"，Go 侧照抄回放，
// 免得我自己脑补响应内容（上一版就是这么凭空注入了一个 Set-Cookie）。
type servedResp struct {
	Status    int     `json:"status"`
	Payload   any     `json:"payload"`
	Cookies   [][]any `json:"cookies"`
	RaiseJSON bool    `json:"raise_json"`
}

type engineGolden struct {
	Argv struct {
		Argv            []string `json:"argv"`
		EnvPATHHead     string   `json:"env_PATH_head"`
		Cwd             string   `json:"cwd"`
		StdoutIsLogFile bool     `json:"stdout_is_logfile"`
	} `json:"argv"`
	ApplyCookies     [][]any           `json:"apply_cookies"`
	OpenPage         map[string]any    `json:"open_page"`
	OpenPageIsolated map[string]any    `json:"open_page_isolated"`
	Renew            map[string]any    `json:"renew_ok"`
	Renew401         map[string]any    `json:"renew_401"`
	Renew403         map[string]any    `json:"renew_403"`
	RenewNoJSON      map[string]any    `json:"renew_200_notjson"`
	RenewPending     map[string]any    `json:"renew_unassigned"`
	RenewNoWake      map[string]any    `json:"renew_no_wake"`
	RenewVMOFF       map[string]any    `json:"renew_vm_disabled"`
	Renew500         map[string]any    `json:"renew_500"`
	Errors           map[string]string `json:"errors"`
}

func sectionServed(section map[string]any) servedResp {
	var s servedResp
	raw, _ := json.Marshal(section["served"])
	_ = json.Unmarshal(raw, &s)
	return s
}

// sectionError 拆出 "MuseAuthError:文案" 形式的金样本错误。
func sectionError(section map[string]any) (kind, msg string, has bool) {
	e, ok := section["error"].(string)
	if !ok || e == "" {
		return "", "", false
	}
	if i := strings.Index(e, ":"); i > 0 {
		return e[:i], e[i+1:], true
	}
	return "", e, true
}

func loadEngineGolden(t *testing.T) engineGolden {
	t.Helper()
	raw := readFileOrFail(t, "testdata/engine_golden.json")
	var g engineGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("金样本解析失败: %v", err)
	}
	if len(g.Errors) > 0 {
		t.Fatalf("金样本自身有错，别拿它当基准: %v", g.Errors)
	}
	return g
}

func readFileOrFail(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skip("缺金样本", p, err)
	}
	return b
}

// renewCall 是金样本里记录的一次 requests 调用。
type renewCall struct {
	M              string            `json:"m"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	Timeout        float64           `json:"timeout"`
	AllowRedirects *bool             `json:"allow_redirects"`
	JSON           map[string]any    `json:"json"`
}

func decodeRenewCalls(t *testing.T, section map[string]any) []renewCall {
	t.Helper()
	out, err := decodeRenewCallsRaw(section["calls"])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeRenewCallsRaw(v any) ([]renewCall, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out []renewCall
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeResult(t *testing.T, section map[string]any) map[string]any {
	t.Helper()
	out, _ := decodeResultRaw(section["result"])
	return out
}

func decodeResultRaw(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- 比较工具

// jsonEquivalent 判断两个值在 JSON 语义下是否相等：忽略对象键顺序，
// 数字按值比较（1700000000 与 1700000000.0 视为相同，因为都是 JSON number）。
func jsonEquivalent(a, b any) bool {
	return reflect.DeepEqual(norm(a), norm(b))
}

func norm(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			out[k] = norm(x)
		}
		return out
	case [2]any:
		return []any{norm(t[0]), norm(t[1])}
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, norm(x))
		}
		return out
	case []string:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, x)
		}
		return out
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return int64(t)
		}
		return t
	case int:
		return int64(t)
	case int64:
		return t
	case nil:
		return nil
	default:
		// 只对基本类型做归一；struct/slice 一律先由调用方 Marshal 成 JSON 再传进来。
		// 这里曾经写成「json.Marshal + Unmarshal + norm」，遇到自引用或非常规类型会无限递归，
		// 直接把测试进程栈打爆（fatal error: stack overflow）。
		return fmt.Sprintf("%#v", t)
	}
}

// cookieSet 把 "a=1; b=2" 拆成排序后的集合串，忽略成员顺序（见 renew.go 的 DEVIATION 注释）。
func cookieSet(h string) string {
	if h == "" {
		return ""
	}
	parts := strings.Split(h, "; ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
