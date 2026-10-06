package cdp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// serveCDP 起一个假 CDP 端点，handler 每收到一条命令就按需回帧（可多条）。
func serveCDP(t *testing.T, handler func(conn *websocket.Conn, msg map[string]any)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			handler(conn, msg)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(h string) string { return "ws" + strings.TrimPrefix(h, "http") }

func TestSendSkipsNonMatchingFrames(t *testing.T) {
	srv := serveCDP(t, func(conn *websocket.Conn, msg map[string]any) {
		conn.WriteJSON(map[string]any{"method": "Network.dataReceived", "params": map[string]any{"x": 1}})
		conn.WriteJSON(map[string]any{"method": "Runtime.consoleAPICalled"})
		conn.WriteJSON(map[string]any{"id": msg["id"], "result": map[string]any{"ok": true}})
	})
	c, err := New(wsURL(srv.URL), 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 服务端在响应前先发了两个事件帧，Python 的语义是吞掉它们并继续等到 id 对上。
	res, err := c.Send("Page.enable", nil, 0)
	if err != nil {
		t.Fatalf("Send 应跨过事件帧拿到响应: %v", err)
	}
	if r := res["result"].(map[string]any); r["ok"] != true {
		t.Fatalf("result=%v", res["result"])
	}
}

func TestSendErrorBecomesCDPErr(t *testing.T) {
	srv := serveCDP(t, func(conn *websocket.Conn, msg map[string]any) {
		conn.WriteJSON(map[string]any{"id": msg["id"], "error": map[string]any{"code": -32000, "message": "boom"}})
	})
	c, _ := New(wsURL(srv.URL), 2*time.Second, 0)
	defer c.Close()

	_, err := c.Send("Runtime.evaluate", nil, 0)
	var e *Err
	if !errors.As(err, &e) {
		t.Fatalf("想要 *Err，得到 %T %v", err, err)
	}
	if e.Method != "Runtime.evaluate" || !strings.Contains(e.Detail, "boom") {
		t.Fatalf("err=%#v", e)
	}
}

func TestSendTimeout(t *testing.T) {
	srv := serveCDP(t, func(conn *websocket.Conn, msg map[string]any) {
		time.Sleep(500 * time.Millisecond) // 故意不回应
	})
	c, _ := New(wsURL(srv.URL), 5*time.Second, 0)
	defer c.Close()

	start := time.Now()
	_, err := c.Send("Page.navigate", nil, 100*time.Millisecond)
	var te *TimeoutErr
	if !errors.As(err, &te) {
		t.Fatalf("想要 *TimeoutErr，得到 %T %v", err, err)
	}
	if waited := time.Since(start); waited < 80*time.Millisecond || waited > 1*time.Second {
		t.Fatalf("超时时机不对: %v", waited)
	}
	if te.Error() != "CDP Page.navigate 超时" {
		t.Fatalf("错误文案变了: %q", te.Error())
	}
}

// TestJSValueAndFallback 锁住 Python 的两段取值语义：
// 有 value 取 value；没有 value 返回的是「外层 result」而不是内层。
func TestJSValueAndFallback(t *testing.T) {
	srv := serveCDP(t, func(conn *websocket.Conn, msg map[string]any) {
		expr, _ := msg["params"].(map[string]any)["expression"].(string)
		if strings.Contains(expr, "WITH_VALUE") {
			conn.WriteJSON(map[string]any{"id": msg["id"], "result": map[string]any{
				"result": map[string]any{"type": "string", "value": "ready"}}})
			return
		}
		conn.WriteJSON(map[string]any{"id": msg["id"], "result": map[string]any{
			"result":           map[string]any{"type": "object", "objectId": "123"},
			"exceptionDetails": map[string]any{"text": "ReferenceError"}}})
	})
	c, _ := New(wsURL(srv.URL), 2*time.Second, 0)
	defer c.Close()

	v, err := c.JS("WITH_VALUE", false, 0)
	if err != nil || v != "ready" {
		t.Fatalf("value 路径: v=%#v err=%v", v, err)
	}

	fb, err := c.JS("NO_VALUE", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := fb.(map[string]any)
	if !ok {
		t.Fatalf("回退应返回外层 result，得到 %#v", fb)
	}
	if _, has := m["exceptionDetails"]; !has {
		t.Fatalf("回退结果应带 exceptionDetails: %#v", m)
	}
}

func TestHTTPJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"webSocketDebuggerUrl":"ws://x/y"}`))
	}))
	defer srv.Close()
	got, err := HTTPJSON(srv.URL, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got["webSocketDebuggerUrl"] != "ws://x/y" {
		t.Fatalf("got=%v", got)
	}
}
