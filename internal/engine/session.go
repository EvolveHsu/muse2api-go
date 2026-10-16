package engine

import (
	"errors"
	"fmt"
	"time"
)

// ErrAuth 对应 MuseAuthError：会话失效，需要重新导入 cookie。
type ErrAuth struct{ Msg string }

func (e *ErrAuth) Error() string { return e.Msg }

// ErrGeneration 对应 MuseGenerationError：生成/加载失败，但不判定会话失效。
type ErrGeneration struct{ Msg string }

func (e *ErrGeneration) Error() string { return e.Msg }

// IsAuthErr 对应 Python 的 `except MuseAuthError: raise`。
func IsAuthErr(err error) bool {
	var a *ErrAuth
	return errors.As(err, &a)
}

// Session 是一条 CDP 会话（浏览器级或页面级），对应 cdp.CDP。
// 抽成接口是为了让测试能拿假实现驱动真逻辑。
type Session interface {
	Send(method string, params map[string]any, timeout time.Duration) (map[string]any, error)
	JS(expr string, awaitPromise bool, timeout time.Duration) (Value, error)
	Close()
}

// RespCookie 对应 requests 的 response.cookies 条目。
type RespCookie struct {
	Name    string
	Value   string
	Expires int64 // 0 = 没有 expires（对应 Python 的 c.expires 为 None/0）
}

// Response 只带 engine 需要的三样：状态码、原始 body、Set-Cookie。
type Response struct {
	StatusCode int
	Body       []byte
	Cookies    []RespCookie
}

// ReqError 的 Kind 是 Python 侧的异常类名，用于复刻 "网络请求失败 (ConnectionError)" 这类文案。
type ReqError struct {
	Kind string
	Err  error
}

func (e *ReqError) Error() string { return fmt.Sprintf("%s: %v", e.Kind, e.Err) }
func (e *ReqError) Unwrap() error { return e.Err }

// HTTPClient 对应 engine.py 里函数内 import 的 requests。
type HTTPClient interface {
	Get(url string, headers map[string]string, timeout time.Duration, allowRedirects bool) (Response, error)
	Post(url string, headers map[string]string, body any, timeout time.Duration) (Response, error)
	Put(url string, headers map[string]string, timeout time.Duration) (Response, error)
}

// RenewResult 对应 renew_session_http 返回的 dict，键名与 JSON 契约一致。
type RenewResult struct {
	OK            bool              `json:"ok"`
	Status        string            `json:"status"`
	VMID          string            `json:"vm_id"`
	VMState       string            `json:"vm_state"`
	WakeOK        bool              `json:"wake_ok"`
	WakeRequested bool              `json:"wake_requested"`
	Cookies       map[string]string `json:"cookies"`
	CookiesExp    map[string]int    `json:"cookies_exp"`
}

// CookieEntry 对应 read_cookies 返回的 {name: {"value":..., "expires":...}}。
type CookieEntry struct {
	Value   string `json:"value"`
	Expires int64  `json:"expires"`
}
