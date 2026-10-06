// access log 与协议层方法表的回放测试。
//
// 两份金样本都来自现役 Python 服务，不手写：
//
//	testdata/accesslog_golden.json    —— 28 条真实 access log 行，含 3 条 %2F / %2e%2e 的规范化差异，
//	                                     其中 7 条是裸 socket 发出去的，排除了「客户端自己改写了 URL」的解释。
//	testdata/method_table.python.json —— 53 个方法名逐个实测的状态码 / content-type / 正文。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type accessEntry struct {
	Probe        string `json:"probe"`
	SentMethod   string `json:"sent_method"`
	SentTarget   string `json:"sent_target"`
	SentAuth     bool   `json:"sent_auth"`
	SentBody     string `json:"sent_body"`
	LoggedStatus int    `json:"logged_status"`
	Line         string `json:"line"`
}

func loadAccessGolden(t *testing.T) (string, []accessEntry) {
	t.Helper()
	raw, err := os.ReadFile("testdata/accesslog_golden.json")
	if err != nil {
		t.Skip("缺 accesslog_golden.json:", err)
	}
	var g struct {
		Prefix  string        `json:"prefix"`
		Entries []accessEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Entries) == 0 {
		t.Fatal("金样本里没有条目")
	}
	return g.Prefix, g.Entries
}

func newAccessApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	a.CFG.AccessLog = true
	buf := &bytes.Buffer{}
	a.accessOut = buf
	return a, buf
}

// statusWriter 必须仍是 Flusher：SSE 那两条路由靠 c.W.(http.Flusher) 拿 flusher，
// 包一层之后若漏了 Flush，断言静默失败 → 流式退化成「攒完再发」，测试里看不出来。
var _ http.Flusher = (*statusWriter)(nil)

// TestAccessLineReplay 把每一条线上请求原样打进 Go 的 ServeHTTP，逐字节比 access log 行。
// 行首（`INFO:` 补白 + client:port）与行身（请求行 + 状态码 + 短语）分开断言：
// 金样本里的 client 是 NAS 的地址与临时端口，回放里固定成 httptest 的地址。
func TestAccessLineReplay(t *testing.T) {
	prefix, entries := loadAccessGolden(t)
	const addr = "192.0.2.1:41234"

	for _, e := range entries {
		t.Run(e.Probe, func(t *testing.T) {
			a, buf := newAccessApp(t)

			req := httptest.NewRequest(e.SentMethod, e.SentTarget, strings.NewReader(e.SentBody))
			req.RemoteAddr = addr
			if e.SentAuth {
				req.Header.Set("authorization", "Bearer cap-key")
			}
			if e.SentBody != "" {
				req.Header.Set("content-type", "application/json")
			}
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)

			if rec.Code != e.LoggedStatus {
				t.Errorf("状态码不同: go=%d py=%d（go body=%.80q）", rec.Code, e.LoggedStatus, rec.Body.String())
			}
			got := buf.String()
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("每条 access log 必须以换行结尾，Go 打了 %q", got)
			}
			got = strings.TrimSuffix(got, "\n")
			wantHead := prefix + addr + " - "
			if head := got[:min(len(got), len(wantHead))]; head != wantHead {
				t.Errorf("行首不同:\n go=%q\nwant=%q", head, wantHead)
			}
			// 行身：金样本那行的 ` - ` 之后（client 部分两边不同，不参与比对）。
			wantTail := e.Line[strings.Index(e.Line, " - ")+len(" - "):]
			i := strings.Index(got, " - ")
			if i < 0 {
				t.Fatalf("Go 打的行里没有分隔符: %q", got)
			}
			if tail := got[i+len(" - "):]; tail != wantTail {
				t.Errorf("请求行/状态不同:\n go=%s\n py=%s", tail, wantTail)
			}
		})
	}
}

// TestAccessLineIPv6 补一条线上没录到的形态：uvicorn 用 f"{client[0]}:{client[1]}" 拼 client，
// IPv6 地址不带方括号，而 Go 的 RemoteAddr 写的是 "[::1]:1234"。
func TestAccessLineIPv6(t *testing.T) {
	a, buf := newAccessApp(t)
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "[::1]:1234"
	a.ServeHTTP(httptest.NewRecorder(), req)

	want := "INFO:     ::1:1234 - \"GET /healthz HTTP/1.1\" 200 OK"
	if got := strings.TrimSuffix(buf.String(), "\n"); got != want {
		t.Errorf("IPv6 的 client 写法不同:\n go=%q\nwant=%q", got, want)
	}
}

// TestAccessLogDisabled 确认 MUSE2API_ACCESS_LOG=0 时一行都不打。
func TestAccessLogDisabled(t *testing.T) {
	a, buf := newAccessApp(t)
	a.CFG.AccessLog = false
	a.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("关掉后不该有输出：%q", buf.String())
	}
}

// TestMethodTableGate 回放 53 个方法名的实测结果：表外的一律 400 纯文本，且不打 access log。
// 这条不是抠字眼 —— Go 的路由把方法转大写再匹配，不拦的话 `post /v1/images/generations`
// 会被当成正经 POST 受理：客户端少敲一个大小写，我们替它花掉一次额度。
func TestMethodTableGate(t *testing.T) {
	raw, err := os.ReadFile("testdata/method_table.python.json")
	if err != nil {
		t.Skip("缺 method_table.python.json:", err)
	}
	var g map[string][]any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g) < 50 {
		t.Fatalf("金样本只有 %d 条方法实测，不够覆盖", len(g))
	}

	accepted, rejected := 0, 0
	for method, want := range g {
		pyStatus := int(want[0].(float64))
		pyType, _ := want[1].(string)
		pyBody, _ := want[2].(string)

		a, buf := newAccessApp(t)
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(method, "/healthz", nil))

		if rec.Code != pyStatus {
			t.Errorf("%s: 状态码 go=%d py=%d", method, rec.Code, pyStatus)
		}
		if got := rec.Header().Get("content-type"); got != pyType {
			t.Errorf("%s: content-type go=%q py=%q", method, got, pyType)
		}
		if pyStatus == 400 {
			rejected++
			if rec.Body.String() != pyBody {
				t.Errorf("%s: 正文 go=%q py=%q", method, rec.Body.String(), pyBody)
			}
			if buf.Len() != 0 {
				t.Errorf("%s: 协议层就挡掉的请求不该打 access log，Go 打了 %q", method, buf.String())
			}
			if llhttpMethods[method] {
				t.Errorf("%s: 实测 400 却仍在 llhttpMethods 表内", method)
			}
			continue
		}
		accepted++
		if !llhttpMethods[method] {
			t.Errorf("%s: 实测能吃下（%d）却被 Go 的方法表拒了", method, pyStatus)
		}
	}
	if accepted != len(llhttpMethods) {
		t.Errorf("方法表写了 %d 条，实测接受 %d 条 —— 对不上就是表抄错了", len(llhttpMethods), accepted)
	}
	if rejected == 0 {
		t.Error("金样本里一条 400 都没有，方法表这条闸门等于没测")
	}
	t.Logf("实测接受 %d 个方法，400 拒掉 %d 个", accepted, rejected)
}
