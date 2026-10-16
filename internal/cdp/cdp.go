// Package cdp 是极简 Chrome DevTools Protocol 客户端，对应 Python 的 cdp.py。
package cdp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Err 对应 Python 的 CDPError。
type Err struct{ Method, Detail string }

func (e *Err) Error() string { return e.Method + ": " + e.Detail }

// TimeoutErr 对应 Python 的 TimeoutError(f"CDP {method} 超时")。
type TimeoutErr struct{ Method string }

func (e *TimeoutErr) Error() string { return fmt.Sprintf("CDP %s 超时", e.Method) }

const (
	defaultTimeout   = 90 * time.Second
	defaultMaxSize   = 256 << 20
	incomingCapacity = 256
)

// CDP 是一条 CDP websocket 会话。
//
// DEVIATION: Python 用同步 socket + recv 超时来轮询；gorilla/websocket 在 read
// deadline 触发后连接即不可复用，所以这里改成「一个后台读协程 + 一个入站 channel」，
// Send 从 channel 里取匹配 id 的响应、丢弃其它消息。可观测语义与 Python 一致
// （非匹配消息同样被丢弃，包括事件帧）。
type CDP struct {
	conn    *websocket.Conn
	writeMu sync.Mutex

	// sendMu 对应上游 1.6.0 给 cdp.py 加的那把 threading.Lock：
	// 同一个连接上「发一条 + 收它的应答」必须整体互斥，否则并发请求会互相把对方的
	// 响应当杂项消息丢掉（Go 这边读协程按 id 匹配，非匹配消息会被直接丢弃）。
	// 多账号并发以后，浏览器级连接（Target.createBrowserContext 等）是跨任务共享的，
	// 这条锁是必需的，不是优化。
	sendMu sync.Mutex

	in        chan map[string]any
	dead      chan struct{}
	timeout   time.Duration
	id        atomic.Int64
	closeOnce sync.Once
}

// New 连接 wsURL；timeout 同时作为整体默认超时与底层 socket 超时的等价物。
func New(wsURL string, timeout time.Duration, maxReadMsgSize int64) (*CDP, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxReadMsgSize <= 0 {
		maxReadMsgSize = defaultMaxSize
	}
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := d.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxReadMsgSize)
	c := &CDP{
		conn:    conn,
		timeout: timeout,
		in:      make(chan map[string]any, incomingCapacity),
		dead:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *CDP) readLoop() {
	defer close(c.dead)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			close(c.in)
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			// Python 里这步发生在 Send 的 except 分支：解析失败就吞掉继续。
			continue
		}
		select {
		case c.in <- msg:
		default:
			// 没人读就丢掉，等价于 Python 里被丢弃的事件消息；不能阻塞读协程。
		}
	}
}

// Send 发送一条命令并等待 id 对上的响应，超时抛 TimeoutErr。
func (c *CDP) Send(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	if timeout <= 0 {
		timeout = c.timeout
	}
	mid := c.id.Add(1)
	payload, err := json.Marshal(map[string]any{"id": mid, "method": method, "params": params})
	if err != nil {
		return nil, err
	}

	c.writeMu.Lock()
	werr := c.conn.WriteMessage(websocket.TextMessage, payload)
	c.writeMu.Unlock()
	if werr != nil {
		return nil, &Err{Method: method, Detail: werr.Error()}
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &TimeoutErr{Method: method}
		}
		var (
			msg map[string]any
			ok  bool
		)
		select {
		case msg, ok = <-c.in:
		case <-c.dead:
			// 读协程退出了，但 channel 里可能还有缓冲消息，先把它排空再判。
			select {
			case msg, ok = <-c.in:
			default:
				ok = false
			}
		case <-time.After(remaining):
			return nil, &TimeoutErr{Method: method}
		}
		if !ok {
			// Python 在连接已死时会把 recv 异常一路吞掉、直到超时才抛；
			// 这里同样等满 deadline 再报超时，保持调用方的阻塞时长一致，但不忙轮询。
			if rem := time.Until(deadline); rem > 0 {
				time.Sleep(rem)
			}
			return nil, &TimeoutErr{Method: method}
		}
		if id, _ := msg["id"].(float64); id != float64(mid) {
			continue
		}
		if e, has := msg["error"]; has {
			return nil, &Err{Method: method, Detail: jsonString(e)}
		}
		return msg, nil
	}
}

// JS 执行表达式；取 result.result.value，没有 value 时回退到整个 result（照抄 Python）。
func (c *CDP) JS(expr string, awaitPromise bool, timeout time.Duration) (any, error) {
	r, err := c.Send("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	}, timeout)
	if err != nil {
		return nil, err
	}
	result, _ := r["result"].(map[string]any)
	inner, _ := result["result"].(map[string]any)
	if v, has := inner["value"]; has {
		return v, nil
	}
	return result, nil
}

// Pump 在 seconds 秒内持续把事件交给回调；期间到达的响应会被丢弃（与 Python 相同）。
func (c *CDP) Pump(seconds float64, onEvent func(map[string]any)) {
	end := time.Now().Add(time.Duration(seconds * float64(time.Second)))
	for {
		remaining := time.Until(end)
		if remaining <= 0 {
			return
		}
		select {
		case msg, ok := <-c.in:
			if !ok {
				return
			}
			if _, hasMethod := msg["method"]; hasMethod && onEvent != nil {
				onEvent(msg)
			}
		case <-time.After(remaining):
			return
		}
	}
}

func (c *CDP) Close() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.closeOnce.Do(func() { _ = c.conn.Close() })
}

// jsonString 把 CDP 的 error 对象序列化，用作错误文案。
// DEVIATION: Python 用 str(dict)（单引号），这里用 JSON；仅影响日志/错误串，不影响对外契约。
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// HTTPJSON 对应 cdp.py 的 http_json()：GET 一个 URL 并把 JSON 解析出来。
func HTTPJSON(url string, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdp: GET %s -> HTTP %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrConnectionClosed 供调用方判连接已断（对应 Python 里 CDPError 的一路用法）。
var ErrConnectionClosed = errors.New("cdp: connection closed")
