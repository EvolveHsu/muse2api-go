package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type genGolden struct {
	Sections map[string]json.RawMessage `json:"sections"`
	Exprs    []string                   `json:"exprs"`
}

func loadGenGolden(t *testing.T) genGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/generate_golden.json")
	if err != nil {
		t.Skip("缺 generate_golden.json:", err)
	}
	var g genGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g genGolden) section(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, ok := g.Sections[name]
	if !ok {
		t.Fatalf("金样本里没有 %s", name)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (g genGolden) logOf(t *testing.T, sec map[string]any) []any {
	t.Helper()
	key := "log"
	if _, ok := sec["log"]; !ok {
		key = "head_log"
	}
	raw, ok := sec[key]
	if !ok {
		t.Fatalf("金样本这一段没有日志")
	}
	b, _ := json.Marshal(raw)
	var list []any
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

// vclock 是虚拟时钟：sleep 不真睡，只把时钟往前推。
// 关键是它按 float64 秒累加，跟金样本脚本里 Python 那个 FakeTime 用的是同一种算术 ——
// 0.6 加 5 次得到 2.9999999999999996 而不是 3.0，于是「3 秒超时」Python 会多轮询一轮。
// 用整纳秒时钟就会少跑那一轮，轮询次数永远对不上；两边跑同一套浮点才算真比对。
type vclock struct{ secs float64 }

func (v *vclock) now() time.Time {
	sec := int64(v.secs)
	return time.Unix(sec, int64((v.secs-float64(sec))*1e9))
}

func (v *vclock) sleep(d time.Duration) { v.secs += d.Seconds() }

// replayPage 严格按金样本记录的调用序列回放：Go 调什么就比对什么，
// 顺序错、表达式差一个字符、参数多一个键都会在这里红。
// 日志用尽后若设了 overflow，则转入「计数模式」——用于 60ms×14s 这类同构轮询，
// 那种日志逐条记下来没有信息量，比次数才等价。
type replayPage struct {
	t        *testing.T
	exprs    []string
	log      []any
	pos      int
	overflow func(expr string) (Value, bool)
	count    func(expr string) string
	counts   map[string]int
	// afterJS 在每次 JS 返回后触发，用来复现金样本打桩 JS 的落盘副作用。
	afterJS func(expr string)
	jsN     int
	sendN   int
}

func newReplay(t *testing.T, g genGolden, sec map[string]any) *replayPage {
	return &replayPage{t: t, exprs: g.Exprs, log: g.logOf(t, sec), counts: map[string]int{}}
}

func (r *replayPage) entry(kind string) ([]any, bool) {
	r.t.Helper()
	if r.pos >= len(r.log) {
		if r.overflow == nil {
			r.t.Fatalf("金样本日志只有 %d 条，Go 又调了一次 %s（多调了）", len(r.log), kind)
		}
		return nil, false
	}
	ent, ok := r.log[r.pos].([]any)
	if !ok {
		r.t.Fatalf("日志第 %d 条不是数组", r.pos)
	}
	r.pos++
	if got, _ := ent[0].(string); got != kind {
		r.t.Fatalf("第 %d 条应是 %s，Go 调的是 %s", r.pos-1, kind, got)
	}
	return ent, true
}

func (r *replayPage) JS(expr string, awaitPromise bool, timeout time.Duration) (Value, error) {
	r.jsN++
	ent, ok := r.entry("js")
	if !ok {
		if lbl := r.label(expr); lbl != "" {
			r.counts[lbl]++
		}
		v, _ := r.overflow(expr)
		return v, nil
	}
	want := r.exprs[int(ent[1].(float64))]
	if expr != want {
		r.t.Errorf("第 %d 条 JS 表达式与 Python 不同:\n got=%.160q\nwant=%.160q", r.pos-1, expr, want)
		return Null(), nil
	}
	if aw, _ := ent[2].(bool); aw != awaitPromise {
		r.t.Errorf("第 %d 条 awaitPromise 不同: go=%v py=%v", r.pos-1, awaitPromise, aw)
	}
	if r.count != nil {
		if lbl := r.count(expr); lbl != "" {
			r.counts[lbl]++
		}
	}
	if raised, _ := ent[4].(bool); raised {
		return Null(), errors.New("boom")
	}
	if r.afterJS != nil {
		defer r.afterJS(expr)
	}
	return toValue(ent[3]), nil
}

func (r *replayPage) label(expr string) string {
	if r.count == nil {
		return ""
	}
	return r.count(expr)
}

func (r *replayPage) Send(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	r.sendN++
	ent, ok := r.entry("send")
	if !ok {
		return map[string]any{"result": map[string]any{}}, nil
	}
	want := ent[1].(string)
	if want != method {
		r.t.Errorf("第 %d 条 send 方法不同: go=%s py=%s", r.pos-1, method, want)
	}
	if ent[2] == nil {
		if params != nil {
			r.t.Errorf("第 %d 条 %s：Python 没传 params，Go 传了 %v", r.pos-1, method, params)
		}
	} else if !jsonSubsetEqual(ent[2].(map[string]any), params) {
		r.t.Errorf("第 %d 条 %s params 不同:\n py=%v\n go=%v", r.pos-1, method, ent[2], params)
	}
	return map[string]any{"result": map[string]any{}}, nil
}

func (r *replayPage) Close() { r.next("close") }

func (r *replayPage) next(kind string) []any {
	ent, _ := r.entry(kind)
	return ent
}

func (r *replayPage) assertDrained(name string) {
	if r.pos != len(r.log) {
		r.t.Errorf("%s：Go 少调了 %d 条 CDP 调用（金样本共 %d）", name, len(r.log)-r.pos, len(r.log))
	}
}

// jsonSubsetEqual：以金样本为准比对 —— Go 的 struct 会把 JS 里不存在的键序列化成 0/false，
// 所以只要求「金样本里出现的键，值一致」。
func jsonSubsetEqual(want any, got any) bool {
	return subsetEqual(want, got, "")
}

func subsetEqual(want, got any, path string) bool {
	switch w := want.(type) {
	case map[string]any:
		gm, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, has := gm[k]
			if !has {
				return false
			}
			if !subsetEqual(wv, gv, path+"."+k) {
				return false
			}
		}
		return true
	case []any:
		gl, ok := got.([]any)
		if !ok || len(w) != len(gl) {
			return false
		}
		for i := range w {
			if !subsetEqual(w[i], gl[i], path) {
				return false
			}
		}
		return true
	case nil:
		return got == nil
	default:
		return jsonEquivalent(want, got)
	}
}

func engineWith(t *testing.T, page Session) (*Engine, *vclock) {
	cfg := testCfg(t)
	e := fixedEngine(t, cfg, &fakeHTTP{})
	e.page = page
	// 起点必须与金样本脚本的 FIXED=[1700000000.0] 一致：浮点累加的相位就从这个数开始。
	vc := &vclock{secs: 1700000000.0}
	e.now = vc.now
	e.sleep = vc.sleep
	return e, vc
}

// mustDirs 建出 data / downloads / media 三个目录。线上由 app 启动时 makedirs，
// 金样本脚本也是这么起手；不建目录的话 _debug_dump 的落盘断言就成了空断言。
func mustDirs(t *testing.T, e *Engine) {
	t.Helper()
	for _, d := range []string{e.CFG.DataDir(), e.CFG.DownloadDir(), e.CFG.MediaDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// errText 把金样本的 "MuseGenerationError:文案" 拆成裸文案。
func errText(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return strings.TrimPrefix(s, "MuseGenerationError:"), true
}

func intOf(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// TestAttachmentsReplay 覆盖正常/坏 JSON/异常三条路径。
func TestAttachmentsReplay(t *testing.T) {
	g := loadGenGolden(t)
	for _, name := range []string{"attachments_valid", "attachments_badjson", "attachments_raise"} {
		t.Run(name, func(t *testing.T) {
			sec := g.section(t, name)
			page := newReplay(t, g, sec)
			e, _ := engineWith(t, page)
			got := e.Attachments()
			gb, _ := json.Marshal(got)
			var asAny any
			_ = json.Unmarshal(gb, &asAny)
			if !subsetEqual(sec["ret"], asAny, name) {
				t.Errorf("Attachments() 结果不同\n py=%v\n go=%v", sec["ret"], string(gb))
			}
			page.assertDrained(name)
		})
	}
}

// TestSendReplay 覆盖 clicked / Enter 兜底（成功与抛错）/ 找不到输入框 / insertText 回退。
// prompt 与 tools/golden/golden_generate.py 里逐条对应；换了 prompt 就会在 setter 表达式比对里红。
func TestSendReplay(t *testing.T) {
	g := loadGenGolden(t)
	cases := []struct {
		name   string
		prompt string
	}{
		{"send_clicked", "你好"},
		{"send_enter_fallback", "你好"},
		{"send_enter_raise", "你好"},
		{"send_no_rect", "你好"},
		{"send_insert_text", "hello"},
		{"send_cjk_200", strings.Repeat("猫", 200)},
		{"send_cjk_600", strings.Repeat("猫", 600)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sec := g.section(t, c.name)
			page := newReplay(t, g, sec)
			e, _ := engineWith(t, page)
			got, err := e.send(c.prompt)
			if want, ok := errText(sec["err"]); ok {
				if err == nil {
					t.Fatalf("Python 抛了 %q，Go 却返回 %q", want, got)
				}
				if err.Error() != want {
					t.Errorf("错误文案不同:\n py=%q\n go=%q", want, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("Go 报错而 Python 没有: %v", err)
				}
				if got != sec["ret"] {
					t.Errorf("send 返回值不同: go=%q py=%v", got, sec["ret"])
				}
			}
			page.assertDrained(c.name)
			if c.name == "send_insert_text" || c.name == "send_cjk_200" {
				if n := countSend(page, "Input.insertText"); n != 1 {
					t.Errorf("setter 没写进去时应当补一次 Input.insertText，实际 %d 次", n)
				}
			}
			if c.name == "send_cjk_600" && countSend(page, "Input.insertText") != 0 {
				t.Errorf("Python 的门槛是 500 个字符，600 字的提示词不该再走 insertText")
			}
		})
	}
}

func countSend(p *replayPage, method string) int {
	n := 0
	for _, ent := range p.log {
		if e, ok := ent.([]any); ok && e[0] == "send" && e[1] == method {
			n++
		}
	}
	return n
}

// pollRule 是 golden_generate.py 里 RULES 那三条评论的 Go 侧镜像（顺序也一致：
// 先 scroll 再 atts 再 state）。特征对不上时计数会变成 0，用例就红，不会静默放过。
func pollRule(expr string) string {
	switch {
	case strings.Contains(expr, "b.scrollHeight-a.scrollHeight"):
		return "scroll"
	case strings.Contains(expr, "seen.add"):
		return "atts"
	case strings.Contains(expr, "slice(-700)"):
		return "state"
	}
	return ""
}

// comparePollCounts 把金样本记的 n_scroll / n_atts / n_state 和虚拟时钟累加值
// 一条条对回去。长轮询用例的「第几轮才接受」就靠这组数字钉住。
func comparePollCounts(t *testing.T, sec map[string]any, page *replayPage, vc *vclock) {
	t.Helper()
	for _, c := range []struct {
		rule string
		key  string
	}{
		{"scroll", "n_scroll"}, {"atts", "n_atts"}, {"state", "n_state"},
	} {
		v, has := sec[c.key]
		if !has {
			continue
		}
		if got := page.counts[c.rule]; got != intOf(v) {
			t.Errorf("%s 调用次数不同: py=%d go=%d", c.key, intOf(v), got)
		}
	}
	if v, has := sec["secs"]; has {
		// 直接比 bit：两边的 sleep 都是同一个 float64 逐次累加，差一位就是累加顺序变了。
		if got := vc.secs - 1700000000.0; got != v.(float64) {
			t.Errorf("虚拟时钟累加相位不同: py=%.17g go=%.17g", v.(float64), got)
		}
	}
}

// TestWaitAttachmentReplay 是最有价值的一组：附件挑选打分、视频兜底宽限期、
// 额度不足、纯文本拒答、媒体报告不误判、基线 src 排除，全部按 Python 的调用序列回放。
// timeout 从金样本里读 —— 那是调用方传进去的，两边不一样循环次数就不可能一致。
func TestWaitAttachmentReplay(t *testing.T) {
	g := loadGenGolden(t)
	cases := []struct {
		name     string
		expect   string
		baseline map[string]bool
	}{
		{name: "wait_att_immediate", expect: "image"},
		{name: "wait_att_video_stable", expect: "video"},
		{name: "wait_att_credits", expect: "image"},
		{name: "wait_att_textonly", expect: "image"},
		{name: "wait_att_media_report", expect: "image"},
		{name: "wait_att_baseline_excluded", expect: "image",
			baseline: map[string]bool{"https://x/same": true}},
		// 视频宽限期：第 27 轮才放行静帧；timeout 只有 10s 时必须一无所获。
		{name: "wait_att_video_grace_fallback", expect: "video"},
		{name: "wait_att_video_grace_not_yet", expect: "video"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sec := g.section(t, c.name)
			page := newReplay(t, g, sec)
			page.count = pollRule
			e, vc := engineWith(t, page)
			att, found, err := e.WaitAttachment("", intOf(sec["timeout"]), c.expect,
				nil, 0, 0, nil, c.baseline)
			if want, ok := errText(sec["err"]); ok {
				if err == nil {
					t.Fatalf("Python 抛了 %q，Go 却拿到附件 %+v", want, att)
				}
				if err.Error() != want {
					t.Errorf("错误文案不同:\n py=%q\n go=%q", want, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("Go 报错而 Python 没有: %v", err)
				}
				if found != (sec["ret"] != nil) {
					t.Fatalf("found=%v，Python 那边 ret=%v", found, sec["ret"])
				}
				if wm, ok := sec["ret"].(map[string]any); ok {
					gb, _ := json.Marshal(att)
					var asAny any
					_ = json.Unmarshal(gb, &asAny)
					if !subsetEqual(wm, asAny, c.name) {
						t.Errorf("附件不同\n py=%v\n go=%v", wm, string(gb))
					}
				}
			}
			comparePollCounts(t, sec, page, vc)
			page.assertDrained(c.name)
		})
	}
}

// TestNegativeTimeoutMeansImmediateTimeout 钉住「负数超时不做钳位」：
// Python 的 generate() 里那个 240 只是形参默认值，路由一律显式传 timeout，
// 所以 client 传 -1 时 deadline 落在过去，循环一次都不跑 —— Go 早前把它钳回 240，等于偷偷多跑 4 分钟。
func TestNegativeTimeoutMeansImmediateTimeout(t *testing.T) {
	g := loadGenGolden(t)
	page := newReplay(t, g, g.section(t, "wait_att_immediate"))
	e, _ := engineWith(t, page)
	att, found, err := e.WaitAttachment("", -1, "image", nil, 0, 0, nil, nil)
	if err != nil || found {
		t.Fatalf("负数超时应立刻返回「没有附件」，Go 给了 found=%v err=%v %+v", found, err, att)
	}
	if page.jsN != 0 {
		t.Errorf("循环一次都不该跑，Go 发了 %d 条 JS", page.jsN)
	}
}

func TestPollAndCountsReplay(t *testing.T) {
	g := loadGenGolden(t)
	t.Run("poll_chat", func(t *testing.T) {
		sec := g.section(t, "poll_chat")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		cnt, txt, stop := e.pollChat()
		want, _ := sec["ret"].([]any)
		if len(want) != 3 || int(want[0].(float64)) != cnt || want[1] != txt || boolOf(want[2]) != stop {
			t.Errorf("pollChat 不同: go=(%d,%q,%v) py=%v", cnt, txt, stop, want)
		}
		page.assertDrained("poll_chat")
	})
	t.Run("agent_count", func(t *testing.T) {
		sec := g.section(t, "agent_count")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		if got := e.agentCount(); got != int(sec["ret"].(float64)) {
			t.Errorf("agentCount: go=%d py=%v", got, sec["ret"])
		}
		page.assertDrained("agent_count")
	})
	t.Run("user_count_none", func(t *testing.T) {
		sec := g.section(t, "user_count_none")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		if got := e.userCount(); got != int(sec["ret"].(float64)) {
			t.Errorf("userCount: go=%d py=%v", got, sec["ret"])
		}
		page.assertDrained("user_count_none")
	})
}

func boolOf(v any) bool { b, _ := v.(bool); return b }

func b64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestExtractBytesReplay(t *testing.T) {
	g := loadGenGolden(t)
	t.Run("extract_ok", func(t *testing.T) {
		sec := g.section(t, "extract_ok")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		data, mime, url, err := e.extractBytes("https://x/a", "image", 4)
		if err != nil {
			t.Fatal(err)
		}
		if b64Of(data) != sec["bytes_b64"] {
			t.Errorf("字节不同: go=%s py=%v", b64Of(data), sec["bytes_b64"])
		}
		if mime != sec["mime"] || url != sec["url"] {
			t.Errorf("mime/url 不同: go=(%q,%q) py=(%v,%v)", mime, url, sec["mime"], sec["url"])
		}
		page.assertDrained("extract_ok")
	})
	t.Run("extract_fail", func(t *testing.T) {
		sec := g.section(t, "extract_fail")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		_, _, _, err := e.extractBytes("https://x/a", "image", 4)
		if err == nil {
			t.Fatal("应当失败")
		}
		if want, _ := errText(sec["err"]); err.Error() != want {
			t.Errorf("错误文案不同:\n py=%v\n go=%q", sec["err"], err.Error())
		}
		if calls, ok := sec["js_calls"]; ok && intOf(calls) != page.jsN {
			t.Errorf("重试次数不同: go=%d py=%v", page.jsN, calls)
		}
		page.assertDrained("extract_fail")
	})
}

// TestChatStreamReplay 走完整链路：reset_thread → 基准 poll → _send → 两段增量循环。
func TestChatStreamReplay(t *testing.T) {
	g := loadGenGolden(t)
	sec := g.section(t, "chat_stream")
	page := newReplay(t, g, sec)
	e, _ := engineWith(t, page)
	e.EnsurePageFn = func(map[string]string, map[string]int, string) (Session, error) { return page, nil }
	var deltas []string
	err := e.ChatStream(map[string]string{"a": "1"}, "hi", nil, intOf(sec["timeout"]), "", nil,
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatalf("Go 报错而 Python 没有: %v", err)
	}
	if strings.Join(deltas, "|") != strings.Join(asSlice(sec["deltas"]), "|") {
		t.Errorf("增量序列不同:\n py=%v\n go=%v", sec["deltas"], deltas)
	}
	page.assertDrained("chat_stream")

	// chat() 就是 chat_stream 的累加，同一段金样本再跑一遍非流式入口。
	t.Run("chat_non_stream", func(t *testing.T) {
		page2 := newReplay(t, g, sec)
		e2, _ := engineWith(t, page2)
		e2.EnsurePageFn = func(map[string]string, map[string]int, string) (Session, error) { return page2, nil }
		got, err := e2.Chat(map[string]string{"a": "1"}, "hi", nil, intOf(sec["timeout"]), "")
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Join(asSlice(sec["deltas"]), ""); got != want {
			t.Errorf("Chat() 全文不同: py=%q go=%q", want, got)
		}
		page2.assertDrained("chat 非流式")
	})
}

func asSlice(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// TestChatStreamConnectingCounts 验的是「14 秒阈值 + 60ms 采样」这套时间算术：
// 金样本记的是 Python 假时钟下的调用次数，Go 用同一个虚拟时钟必须落在同样的次数上。
func TestChatStreamConnectingCounts(t *testing.T) {
	g := loadGenGolden(t)
	sec := g.section(t, "chat_stream_connecting")
	page := newReplay(t, g, sec)

	var stateExpr, tailExpr string
	for _, e := range g.Exprs {
		if strings.Contains(e, "cnt:nonEmpty.length,total:bs.length") {
			stateExpr = e
		}
		if strings.Contains(e, "document.body.innerText.slice(-500)") &&
			strings.TrimSpace(e) == "document.body.innerText.slice(-500)" {
			tailExpr = e
		}
	}
	if stateExpr == "" || tailExpr == "" {
		t.Fatalf("金样本表达式表里找不到 poll/tail 两条 JS（state=%q tail=%q 是否为空）", stateExpr, tailExpr)
	}
	page.count = func(expr string) string {
		switch expr {
		case stateExpr:
			return "state"
		case tailExpr:
			return "tail"
		}
		return ""
	}
	page.overflow = func(expr string) (Value, bool) {
		switch expr {
		case stateExpr:
			return StrV(`{"cnt": 0, "txt": "", "stop": false, "tail": ""}`), true
		case tailExpr:
			return StrV("Connecting..."), true
		}
		page.t.Errorf("轮询溢出后出现了预期外的 JS: %.120q", expr)
		return Null(), false
	}

	e, _ := engineWith(t, page)
	e.EnsurePageFn = func(map[string]string, map[string]int, string) (Session, error) { return page, nil }
	err := e.ChatStream(map[string]string{"a": "1"}, "hi", nil, intOf(sec["timeout"]), "", nil,
		func(string) error { return nil })
	want, ok := errText(sec["err"])
	if !ok {
		t.Fatal("金样本这一段没有错误")
	}
	if err == nil || err.Error() != want {
		t.Fatalf("错误不同:\n py=%q\n go=%v", want, err)
	}
	if got := intOf(sec["n_state"]); page.counts["state"] != got {
		t.Errorf("状态轮询次数不同: go=%d py=%d", page.counts["state"], got)
	}
	if got := intOf(sec["n_tail"]); page.counts["tail"] != got {
		t.Errorf("tail 轮询次数不同: go=%d py=%d", page.counts["tail"], got)
	}
	if got := intOf(sec["n_total"]); page.jsN+page.sendN != got {
		t.Errorf("总调用次数不同: go=%d py=%d", page.jsN+page.sendN, got)
	}
}

// TestClearAndAttachReplay 覆盖参考图链路：清遗留附件 → DataTransfer 注入 → 上传确认轮询。
// inject 那条 JS 是 pyFormat+jsonString 的产物，表达式逐字节比对 = 顺带验了转义实现。
func TestClearAndAttachReplay(t *testing.T) {
	g := loadGenGolden(t)
	const ref = "data:image/png;base64,iVBORw0KGgo="

	t.Run("clear_attachments", func(t *testing.T) {
		sec := g.section(t, "clear_attachments")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		e.clearAttachments()
		page.assertDrained("clear_attachments")
	})

	for _, c := range []struct{ name, input string }{
		{"attach_image_ok", ref},
		{"attach_image_notok", ref},
		{"attach_image_unconfirmed", ref},
		{"attach_image_nob64", "data:image/png;base64,"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sec := g.section(t, c.name)
			page := newReplay(t, g, sec)
			e, _ := engineWith(t, page)
			err := e.attachImage(c.input)
			want, hasWant := errText(sec["err"])
			if hasWant {
				if err == nil || err.Error() != want {
					t.Fatalf("错误不同:\n py=%q\n go=%v", want, err)
				}
			} else if err != nil {
				t.Fatalf("Go 报错而 Python 没有: %v", err)
			}
			page.assertDrained(c.name)
		})
	}
}

// TestDownloadFallbackReplay 验「只认这个结果自己的下载按钮」以及
// 新文件挑选规则：忽略 .crdownload，取 mtime 最新的那个。
func TestDownloadFallbackReplay(t *testing.T) {
	g := loadGenGolden(t)
	dlEpochs := []struct {
		name string
		sec  int64
		body string
	}{
		{"new.png", 1700000000, "NEW"},
		{"new2.png", 1800000000, "NEW2BYTES"},
		{"zz.png.crdownload", 1900000000, "PART"},
	}

	t.Run("dl_none", func(t *testing.T) {
		sec := g.section(t, "dl_none")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		mustDirs(t, e)
		got, err := e.downloadFallback("https://x/a", 180)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("没有本结果的下载按钮时应当什么都不返回，得到 %q", got)
		}
		page.assertDrained("dl_none")
	})

	t.Run("dl_ok", func(t *testing.T) {
		sec := g.section(t, "dl_ok")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		mustDirs(t, e)
		dir := e.CFG.DownloadDir()
		// 呼叫前就存在的旧文件不能当新结果（Python 把它记进了 before）。
		old := filepath.Join(dir, "old.png")
		if err := os.WriteFile(old, []byte("OLD"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(old, time.Unix(1600000000, 0), time.Unix(1600000000, 0)); err != nil {
			t.Fatal(err)
		}
		fired := false
		page.afterJS = func(string) {
			if fired {
				return
			}
			fired = true
			for _, f := range dlEpochs {
				p := filepath.Join(dir, f.name)
				if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
					page.t.Fatal(err)
				}
				ts := time.Unix(f.sec, 0)
				if err := os.Chtimes(p, ts, ts); err != nil {
					page.t.Fatal(err)
				}
			}
		}
		got, err := e.downloadFallback("https://x/a", intOf(sec["timeout"]))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.TrimPrefix(asString2(sec["ret"]), "<DL>")
		if filepath.Base(got) != strings.TrimPrefix(want, "/") {
			t.Errorf("挑中的文件不同:\n py=%q\n go=%q", sec["ret"], got)
		}
		if got != "" && filepath.Dir(got) != dir {
			t.Errorf("返回的路径没落在下载目录里: %q", got)
		}
		page.assertDrained("dl_ok")
	})
}

// TestDebugDumpReplay 验 _debug_dump 落盘的字节，以及 JS 抛错时必须什么都不写。
func TestDebugDumpReplay(t *testing.T) {
	g := loadGenGolden(t)
	t.Run("debug_dump", func(t *testing.T) {
		sec := g.section(t, "debug_dump")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		mustDirs(t, e)
		e.debugDump("unit")
		raw, err := os.ReadFile(filepath.Join(e.CFG.DataDir(), "debug-unit.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != asString2(sec["content"]) {
			t.Errorf("调试文件内容不同:\n py=%q\n go=%q", sec["content"], string(raw))
		}
		page.assertDrained("debug_dump")
	})
	t.Run("debug_dump_raise", func(t *testing.T) {
		sec := g.section(t, "debug_dump_raise")
		page := newReplay(t, g, sec)
		e, _ := engineWith(t, page)
		mustDirs(t, e)
		e.debugDump("raise")
		_, err := os.Stat(filepath.Join(e.CFG.DataDir(), "debug-raise.json"))
		if (err == nil) != boolOf(sec["exists"]) {
			t.Errorf("JS 抛错时不该写文件：py exists=%v, go exists=%v", sec["exists"], err == nil)
		}
		page.assertDrained("debug_dump_raise")
	})
}

// TestGenerateFullReplay 是 Stage C 的总账：generate() 整条链路的 CDP 调用序列、
// 返回字段、落盘字节全部对齐真 Python。带参考图那一条还钉住了
// 「先挂图、再记基线」的顺序 —— 反了就会把旧历史图当基线，界面留下上一张的结果。
func TestGenerateFullReplay(t *testing.T) {
	g := loadGenGolden(t)
	cases := []struct {
		name    string
		expect  string
		prompt  string
		ref     string
		timeout int
		dlFile  bool
	}{
		{"generate_full_image", "image", "画猫", "", 60, false},
		{"generate_full_image_ref", "image", "画猫", "data:image/png;base64,iVBORw0KGgo=", 60, false},
		{"generate_full_download_fallback", "image", "画猫", "", 60, true},
		{"generate_timeout", "image", "画猫", "", 3, false},
		// 视频三条分支：真出视频 / 只有封面静帧（线上「2 秒视频」的实际结局）/
		// mime 与 URL 都认不出时由 expect 兜底，外加宽限期没到就必须放弃的那条。
		{"generate_full_video", "video", "红球弹起", "", 60, false},
		{"generate_full_video_ext_from_expect", "video", "红球弹起", "", 60, false},
		{"generate_full_video_poster_only", "video", "红球弹起", "", 60, false},
		{"generate_full_video_gives_up", "video", "红球弹起", "", 10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sec := g.section(t, c.name)
			page := newReplay(t, g, sec)
			page.count = pollRule
			e, vc := engineWith(t, page)
			mustDirs(t, e)
			if c.dlFile {
				// 金样本里点下载按钮的那次 JS 调用会在 downloads 里落下 shot.png；
				// Go 必须在同一个时刻造出它，listdir 的差分才比得出「这是新文件」。
				fired := false
				page.afterJS = func(expr string) {
					if fired || !strings.Contains(expr, "下载|保存|download") {
						return
					}
					fired = true
					p := filepath.Join(e.CFG.DownloadDir(), "shot.png")
					if err := os.WriteFile(p, []byte("PNGBYTES"), 0o644); err != nil {
						page.t.Fatal(err)
					}
					ts := time.Unix(1750000000, 0)
					if err := os.Chtimes(p, ts, ts); err != nil {
						page.t.Fatal(err)
					}
				}
			}
			e.EnsurePageFn = func(map[string]string, map[string]int, string) (Session, error) { return page, nil }
			res, err := e.Generate(map[string]string{"a": "1"}, c.prompt, c.expect,
				c.timeout, nil, "", nil, c.ref, nil)
			comparePollCounts(t, sec, page, vc)
			if want, ok := errText(sec["err"]); ok {
				if err == nil || err.Error() != want {
					t.Fatalf("错误不同:\n py=%q\n go=%v", want, err)
				}
				if p, ok := sec["dump_file"]; ok {
					_, statErr := os.Stat(filepath.Join(e.CFG.DataDir(), "debug-no-attachment.json"))
					if (statErr == nil) != boolOf(p) {
						t.Errorf("超时应当留下 debug-no-attachment.json：py=%v", p)
					}
				}
				page.assertDrained(c.name)
				return
			}
			if err != nil {
				t.Fatalf("Go 报错而 Python 没有: %v", err)
			}
			for key, want := range map[string]any{"via": res.Via, "ext": res.Ext, "kind": res.Kind,
				"size": float64(res.Size), "attachment": res.Attachment, "mime": res.MimeType,
				"w": float64(res.W), "h": float64(res.H)} {
				if sec[key] != nil && fmt.Sprint(sec[key]) != fmt.Sprint(want) {
					t.Errorf("%s 不同: py=%v go=%v", key, sec[key], want)
				}
			}
			// 文件名形状与落盘字节只有录了这些字段的那几段才比对。
			if sec["name_len"] != nil {
				if len(res.Filename) != intOf(sec["name_len"]) {
					t.Errorf("文件名的形状不同: py=%d(%v) go=%q", intOf(sec["name_len"]),
						sec["name_tail"], res.Filename)
				}
				if !strings.HasSuffix(res.Filename, asString2(sec["name_tail"])) {
					t.Errorf("扩展名不同: py=%v go=%q", sec["name_tail"], res.Filename)
				}
			}
			if !strings.HasPrefix(res.Path, e.CFG.MediaDir()) {
				t.Errorf("结果没落进 media 目录: %q", res.Path)
			}
			if sec["bytes_b64"] != nil {
				body, readErr := os.ReadFile(res.Path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if b64Of(body) != sec["bytes_b64"] {
					t.Errorf("落盘字节不同: py=%v go=%s", sec["bytes_b64"], b64Of(body))
				}
			}
			if want, ok := sec["ret_keys"].([]any); ok {
				gb, _ := json.Marshal(res)
				m := map[string]any{}
				_ = json.Unmarshal(gb, &m)
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if strings.Join(keys, ",") != strings.Join(asSlice(want), ",") {
					t.Errorf("返回字段集合不同:\n py=%v\n go=%v", want, keys)
				}
			}
			page.assertDrained(c.name)
		})
	}
}

func TestQuotaReplay(t *testing.T) {
	g := loadGenGolden(t)
	cases := []struct{ name, wantErr string }{
		{"quota", ""},
		{"quota_no_button", "找不到 Settings 按钮"},
		{"quota_no_menu", "Settings 菜单未弹出"},
		{"quota_no_text", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sec := g.section(t, c.name)
			page := newReplay(t, g, sec)
			e, _ := engineWith(t, page)
			e.EnsurePageFn = func(map[string]string, map[string]int, string) (Session, error) { return page, nil }
			got, err := e.Quota(map[string]string{"a": "1"}, nil)
			if want, ok := errText(sec["err"]); ok {
				if err == nil || err.Error() != want {
					t.Fatalf("错误不同:\n py=%q\n go=%v", want, err)
				}
				page.assertDrained(c.name)
				return
			}
			if err != nil {
				t.Fatalf("Go 报错而 Python 没有: %v", err)
			}
			want := sec["ret"].(map[string]any)
			gb, _ := json.Marshal(got)
			var asAny any
			_ = json.Unmarshal(gb, &asAny)
			if !subsetEqual(want, asAny, c.name) || len(want) != len(got) {
				t.Errorf("quota 结果不同\n py=%v\n go=%s", want, gb)
			}
			page.assertDrained(c.name)
		})
	}
	// 面板文本里带 "plan" 与 "N% used"，found 必须为 true
	sec := g.section(t, "quota")
	if ret, ok := sec["ret"].(map[string]any); ok && boolOf(ret["found"]) != true {
		t.Errorf("金样本这一段的 found 预期为 true：%v", ret["found"])
	}
}

// TestPureFunctionsAgainstGolden 用真 Python 的输出验纯函数。
func TestPureFunctionsAgainstGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/engine_golden.json")
	if err != nil {
		t.Skip("缺 engine_golden.json:", err)
	}
	var g map[string]any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}

	if rows, ok := g["pick_ext"].([]any); ok {
		for _, r := range rows {
			row := r.([]any)
			mimeType, url, expect := asString2(row[0]), asString2(row[1]), asString2(row[2])
			want := asString2(row[3])
			if got := pickExt(mimeType, url, expect); got != want {
				t.Errorf("pickExt(%q,%q,%q): go=%q py=%q", mimeType, url, expect, got, want)
			}
		}
	} else {
		t.Error("金样本里没有 pick_ext")
	}

	for _, key := range []string{"parse_quota", "parse_quota_empty", "parse_quota_weird"} {
		want, ok := g[key].(map[string]any)
		if !ok {
			t.Errorf("金样本里没有 %s", key)
			continue
		}
		txt := ""
		switch key {
		case "parse_quota":
			txt = "Free plan\nWeekly limit resets on Sep 30\n1% used\nAdditional tokens\nNever expires\n0% used (2B tokens left)"
		case "parse_quota_empty":
			txt = ""
		case "parse_quota_weird":
			txt = "Pro plan\n3% used (500M tokens left)\nNever expires\nweekly limit resets on Jan 1"
		}
		got := parseQuota(txt)
		gb, _ := json.Marshal(got)
		var asAny any
		_ = json.Unmarshal(gb, &asAny)
		if !subsetEqual(want, asAny, key) || len(want) != len(got) {
			t.Errorf("%s 不同\n py=%v\n go=%s", key, want, gb)
		}
	}

	norm, ok := g["normalize"].(map[string]any)
	if !ok {
		t.Error("金样本里没有 normalize")
		return
	}
	e, _ := engineWith(t, &replayPage{t: t, exprs: []string{}, counts: map[string]int{}})
	e.Fetch = func(u string) ([]byte, string, error) {
		return []byte{0x00, 0x01, 'b', 'i', 'n', 'a', 'r', 'y'}, "image/png", nil
	}
	checkNorm := func(section, input string) {
		want, ok := norm[section].([]any)
		if !ok {
			t.Errorf("金样本里没有 normalize.%s", section)
			return
		}
		gotB64, gotMime := e.normalizeImage(input)
		if gotB64 != want[0] || gotMime != want[1] {
			t.Errorf("normalizeImage(%q): go=(%q,%q) py=(%v,%v)", input, gotB64, gotMime, want[0], want[1])
		}
	}
	checkNorm("empty", "")
	checkNorm("raw_b64", "iVBORw0KGgo=")
	checkNorm("data_plain", "data:,abc")
	checkNorm("data_mime", "data:image/gif;base64,R0lGOD")
	checkNorm("data_mime_nob64", "data:image/jpeg,XX")
}
