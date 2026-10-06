package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"muse2api-go/internal/cdp"
)

// liveSession 把 cdp.CDP 适配成 Session，并把回传值转成 Value（保住 Python 的真假语义）。
type liveSession struct{ c *cdp.CDP }

func newRealSession(wsURL string, timeout time.Duration) (Session, error) {
	c, err := cdp.New(wsURL, timeout, 0)
	if err != nil {
		return nil, err
	}
	return &liveSession{c: c}, nil
}

func (s *liveSession) Send(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	return s.c.Send(method, params, timeout)
}

func (s *liveSession) JS(expr string, awaitPromise bool, timeout time.Duration) (Value, error) {
	v, err := s.c.JS(expr, awaitPromise, timeout)
	if err != nil {
		return Null(), err
	}
	return toValue(v), nil
}

func (s *liveSession) Close() { s.c.Close() }

// toValue 把 encoding/json 解出来的 any 变成 Value。
// 数字统一走 float64，Bits 用 jsonNumber 还原成 Python 的写法（整数值不带小数点）。
func toValue(v any) Value {
	switch t := v.(type) {
	case nil:
		return Null()
	case bool:
		return BoolV(t)
	case string:
		return StrV(t)
	case float64:
		return NumF(t)
	case json.Number:
		f, _ := t.Float64()
		return Value{Kind: KNumber, Num: f, Bits: t.String()}
	case map[string]any:
		out := make(map[string]Value, len(t))
		for k, x := range t {
			out[k] = toValue(x)
		}
		return Value{Kind: KObject, Obj: out}
	case []any:
		out := make([]Value, 0, len(t))
		for _, x := range t {
			out = append(out, toValue(x))
		}
		return Value{Kind: KArray, Array: out}
	}
	return StrV(fmt.Sprint(v))
}

// liveHTTP 是 engine.HTTPClient 的真实实现，对应 Python 的 requests。
type liveHTTP struct{ client *http.Client }

func NewLiveHTTP() HTTPClient {
	return &liveHTTP{client: &http.Client{}}
}

func (h *liveHTTP) do(method, url string, headers map[string]string, timeout time.Duration,
	allowRedirects bool, body any) (Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return Response{}, &ReqError{Kind: "ValueError", Err: err}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return Response{}, &ReqError{Kind: requestsKind(err), Err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cl := h.client
	if timeout > 0 {
		cl = &http.Client{Timeout: timeout, CheckRedirect: redirectPolicy(allowRedirects)}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return Response{}, &ReqError{Kind: requestsKind(err), Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &ReqError{Kind: "ConnectionError", Err: err}
	}
	out := Response{StatusCode: resp.StatusCode, Body: raw}
	for _, c := range resp.Cookies() {
		exp := int64(0)
		if !c.Expires.IsZero() {
			exp = c.Expires.Unix()
		} else if c.MaxAge > 0 {
			exp = time.Now().Unix() + int64(c.MaxAge)
		}
		out.Cookies = append(out.Cookies, RespCookie{Name: c.Name, Value: c.Value, Expires: exp})
	}
	return out, nil
}

func redirectPolicy(allow bool) func(*http.Request, []*http.Request) error {
	if allow {
		return nil
	}
	return func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
}

func (h *liveHTTP) Get(url string, headers map[string]string, timeout time.Duration, allowRedirects bool) (Response, error) {
	return h.do(http.MethodGet, url, headers, timeout, allowRedirects, nil)
}

func (h *liveHTTP) Post(url string, headers map[string]string, body any, timeout time.Duration) (Response, error) {
	return h.do(http.MethodPost, url, headers, timeout, true, body)
}

func (h *liveHTTP) Put(url string, headers map[string]string, timeout time.Duration) (Response, error) {
	return h.do(http.MethodPut, url, headers, timeout, true, nil)
}

// requestsKind 给出与 Python requests 异常同名的类别，错误文案要一致。
func requestsKind(err error) string {
	s := err.Error()
	switch {
	case bytes.Contains([]byte(s), []byte("no such host")),
		bytes.Contains([]byte(s), []byte("dial")),
		bytes.Contains([]byte(s), []byte("connection refused")):
		return "ConnectionError"
	case bytes.Contains([]byte(s), []byte("timeout")):
		return "Timeout"
	default:
		return "RequestException"
	}
}

// 保留：CookieEntry 的整数字段在 store 侧要能直接进 cookies_exp。
func cookieExpToInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}
