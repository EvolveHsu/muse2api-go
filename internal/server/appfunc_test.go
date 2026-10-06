package server

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"muse2api-go/internal/pyval"
)

// 这份金样本是 tools/golden/golden_appfunc.py 在容器里**直接调用 app.py 的纯函数**录出来的：
// 提示词拼装、messages 归一、工具调用解析、SSE 帧、分片节奏。
// 全是字符串活，最容易被「看着一样」骗过去，所以逐字节比，不做包含关系之类的弱断言。
func loadAppFunc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/appfunc_golden.json")
	if err != nil {
		t.Skip("缺 appfunc_golden.json:", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留 Python 的 int/float 之分，否则 10 会变成 10.0 喂进校验器
	var g map[string]any
	if err := dec.Decode(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func strPtr(s string) *string { return &s }

func TestResolveModelGolden(t *testing.T) {
	g := loadAppFunc(t)
	for _, row := range arr(g["resolve_model"]) {
		r := row.([]any)
		var name *string
		if r[0] != nil {
			s, _ := r[0].(string)
			name = &s
		}
		defImage := resolveModel(name, "muse-image")
		defSpark := resolveModel(name, "muse-spark")
		if defImage != r[1] {
			t.Errorf("resolve_model(%v) = %q，Python 给 %q", r[0], defImage, r[1])
		}
		if defSpark != r[2] {
			t.Errorf("resolve_model(%v, muse-spark) = %q，Python 给 %q", r[0], defSpark, r[2])
		}
	}
	if int(num(g["alias_count"])) != len(modelAliases) {
		t.Errorf("别名条数不同: go=%d py=%v", len(modelAliases), g["alias_count"])
	}
	got := make([]string, 0)
	for _, k := range sortedAliasKeys() {
		got = append(got, k)
	}
	want := arr(g["alias_keys_sample"])
	if len(want) > len(got) {
		t.Fatalf("别名表比 Python 少")
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("排序后的别名不同 @%d: go=%q py=%v", i, got[i], w)
		}
	}
}

func sortedAliasKeys() []string {
	keys := make([]string, 0, len(modelAliases))
	for k := range modelAliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestBuildImagePromptGolden(t *testing.T) {
	g := loadAppFunc(t)
	for i, row := range arr(g["build_image_prompt"]) {
		r := row.([]any)
		body, _ := r[0].(map[string]any)
		req, errs := parseImageRequest(body)
		if len(errs) > 0 {
			t.Fatalf("#%d 解析报错：%v", i, validationRepr(errs))
		}
		if got := buildImagePrompt(req); got != r[1] {
			t.Errorf("build_image_prompt(#%d %v) 不同:\n go=%q\n py=%q", i, r[0], got, r[1])
		}
		if dumped, ok := r[2].(map[string]any); ok {
			if !sameShapePy(dumpImage(req), dumped) {
				t.Errorf("model_dump(by_alias=True) 不同 #%d:\n go=%s\n py=%v",
					i, dumpsJSON(dumpImage(req), false), dumped)
			}
		}
	}
}

func TestBuildVideoPromptGolden(t *testing.T) {
	g := loadAppFunc(t)
	for i, row := range arr(g["build_video_prompt"]) {
		r := row.([]any)
		body, _ := r[0].(map[string]any)
		req, errs := parseVideoRequest(body)
		if len(errs) > 0 {
			t.Fatalf("#%d 解析报错：%v", i, validationRepr(errs))
		}
		if got := buildVideoPrompt(req); got != r[1] {
			t.Errorf("build_video_prompt(#%d %v) 不同:\n go=%q\n py=%q", i, r[0], got, r[1])
		}
	}
}

func TestContentTextGolden(t *testing.T) {
	// 与采集脚本里 CT 列表逐项对应（脚本记的是输入值的 repr，所以这里按位置配对）。
	inputs := []any{nil, "纯文本", []any{}, []any{"a", "b"},
		[]any{map[string]any{"type": "text", "text": "T"}},
		[]any{map[string]any{"type": nil, "text": "无类型"}},
		[]any{map[string]any{"type": "input_text", "text": "I"}},
		[]any{map[string]any{"type": "output_text", "text": "O"}},
		[]any{map[string]any{"type": "image_url",
			"image_url": map[string]any{"url": "u"}}},
		[]any{map[string]any{"type": "text"}},
		[]any{map[string]any{"nope": json.Number("1")}},
		[]any{json.Number("123")},
		[]any{map[string]any{"text": json.Number("5")}},
		"不是列表的内容"}
	g := loadAppFunc(t)
	rows := arr(g["content_text"])
	if len(rows) != len(inputs) {
		t.Fatalf("用例数不同: go=%d py=%d", len(inputs), len(rows))
	}
	for i, in := range inputs {
		want := rows[i].([]any)[1]
		if got := contentText(in); got != want {
			t.Errorf("_content_text(#%d %v) = %q，Python 给 %v", i, in, got, want)
		}
	}
}

func TestBuildChatPromptGolden(t *testing.T) {
	chatCases := []any{
		[]any{},
		[]any{msg("user", "你好", nil)},
		[]any{msg("user", "  你好  ", nil)},
		[]any{msg("system", "你是助手", nil), msg("user", "你好", nil)},
		[]any{msg("developer", "D", nil), msg("user", "你好", nil)},
		[]any{msg("user", "一", nil), msg("assistant", "二", nil), msg("user", "三", nil)},
		[]any{msg("tool", "结果", nil)},
		[]any{msg("tool", "", nil)},
		[]any{withToolCalls("assistant", []any{
			map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "f", "arguments": "{}"}}})},
		[]any{withToolCalls("assistant", []any{map[string]any{"id": "c1"}})},
		[]any{msg("user", []any{map[string]any{"type": "text", "text": "多模态"},
			map[string]any{"type": "image_url",
				"image_url": map[string]any{"url": "u"}}}, nil)},
		[]any{msg("SYSTEM", "危险 danger-full-access 模式", nil), msg("user", "跑一下", nil)},
		[]any{msg("system", "A", nil), msg("system", "B", nil), msg("user", "U", nil)},
		[]any{msg("user", "", nil)},
		[]any{msg("", "没角色", nil)},
	}
	g := loadAppFunc(t)
	rows := arr(g["build_chat_prompt"])
	if len(rows) != len(chatCases) {
		t.Fatalf("用例数不同: go=%d py=%d", len(chatCases), len(rows))
	}
	for i, c := range chatCases {
		raw := c.([]any)
		messages := make([]ChatMessage, 0, len(raw))
		for _, m := range raw {
			pair := m.([]any)
			cm := ChatMessage{Role: pair[0].(string), Content: pair[1]}
			var tc any
			if len(pair) > 2 {
				tc = pair[2]
			}
			if tc != nil {
				cm.ToolCalls = tc
			}
			messages = append(messages, cm)
		}
		want := rows[i].([]any)
		got := buildChatPrompt(messages)
		if !samePrompt(got, want[1], i) {
			t.Errorf("build_chat_prompt(#%d) 不同:\n go=%q\n py=%q", i, got, want[1])
		}
		if want[2] != nil && int(num(want[2])) != len([]rune(got)) {
			t.Errorf("build_chat_prompt(#%d) 字符长度不同: go=%d py=%v", i, len([]rune(got)), want[2])
		}
	}
}

func msg(role string, content any, _ any) []any {
	return []any{role, content}
}

func withToolCalls(role string, calls any) []any {
	return []any{role, nil, calls}
}

func TestResponsesMessagesGolden(t *testing.T) {
	g := loadAppFunc(t)
	for i, row := range arr(g["responses_messages"]) {
		r := row.([]any)
		body, _ := r[0].(map[string]any)
		req, errs := parseResponsesRequest(body)
		if len(errs) > 0 {
			t.Fatalf("#%d 解析报错：%v", i, validationRepr(errs))
		}
		ms := responsesMessages(req)
		gotPairs := make([]any, 0, len(ms))
		for _, m := range ms {
			gotPairs = append(gotPairs, []any{m.Role, m.Content})
		}
		if !sameShapePy(gotPairs, r[1]) {
			t.Errorf("_responses_messages(#%d %v) 不同:\n go=%s\n py=%v",
				i, r[0], dumpsJSON(gotPairs, false), r[1])
		}
		if got := buildChatPrompt(ms); got != r[2] {
			t.Errorf("build_chat_prompt(responses #%d) 不同:\n go=%q\n py=%q", i, got, r[2])
		}
	}
}

func TestParseToolCallsGolden(t *testing.T) {
	g := loadAppFunc(t)
	for _, row := range arr(g["parse_tool_calls"]) {
		r := row.([]any)
		text, _ := r[0].(string)
		calls, rest := parseToolCalls(text)
		var ser any
		if calls != nil {
			ser = pyval.Dumps(callsAsAny(calls), false, false)
		}
		wantCalls := r[1]
		if wantCalls == nil {
			if ser != nil {
				t.Errorf("parse_tool_calls(%q) Go 解析出了工具调用 %v，Python 没有", text, ser)
			}
		} else {
			s, _ := ser.(string)
			if maskIDs(s) != wantCalls {
				t.Errorf("parse_tool_calls(%q) 调用不同:\n go=%v\n py=%v", text, maskIDs(s), wantCalls)
			}
		}
		if rest != r[2] {
			t.Errorf("parse_tool_calls(%q) 剩余文本不同:\n go=%q\n py=%q", text, rest, r[2])
		}
	}
}

func maskIDs(s string) string {
	return idRe.ReplaceAllString(s, "<id>")
}

func callsAsAny(calls []pyval.Obj) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, c)
	}
	return out
}

func TestToolsPromptGolden(t *testing.T) {
	// 与采集脚本里 TOOLDEF 逐项对应。
	inputs := []any{nil, []any{}, []any{json.Number("123")},
		[]any{map[string]any{"name": "f"}},
		[]any{map[string]any{"name": "f", "description": "d",
			"parameters": map[string]any{"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "string", "description": "参数A"},
					"b": map[string]any{"type": "integer"},
					"c": map[string]any{}},
				"required": []any{"a"}}}},
		[]any{map[string]any{"type": "function", "function": map[string]any{
			"name": "g", "parameters": map[string]any{"properties": map[string]any{}}}}},
		[]any{map[string]any{"function": map[string]any{"name": "h",
			"parameters": map[string]any{"properties": map[string]any{
				"z": map[string]any{"description": "只有描述"}}}}}},
		[]any{map[string]any{"nope": json.Number("1")}, map[string]any{"name": ""}},
	}
	g := loadAppFunc(t)
	rows := arr(g["tools_prompt"])
	if len(rows) != len(inputs) {
		t.Fatalf("用例数不同: go=%d py=%d", len(inputs), len(rows))
	}
	for i, in := range inputs {
		want := rows[i].([]any)[1]
		if got := buildToolsPrompt(in); got != want {
			t.Errorf("build_tools_prompt(#%d) 不同:\n go=%q\n py=%q", i, got, want)
		}
	}
	if g["tool_protocol_head"] != toolProtocolHead {
		t.Errorf("工具协议头模板不同:\n go=%q\n py=%q", toolProtocolHead, g["tool_protocol_head"])
	}
}

func TestSSEAndChunkGolden(t *testing.T) {
	g := loadAppFunc(t)
	want := arr(g["sse"])
	// 与脚本里那三个对象逐项对应。
	objs := []any{
		pyval.Obj{{"a", json.Number("1")}, {"中文", "值"}, {"n", nil}},
		pyval.Obj{{"data", []any{pyval.Obj{{"x", "<>&"}}}}},
		pyval.Obj{},
	}
	if len(want) != len(objs) {
		t.Fatalf("SSE 用例数不同")
	}
	for i, o := range objs {
		if got := sse(o); got != want[i] {
			t.Errorf("sse(#%d) 不同:\n go=%q\n py=%v", i, got, want[i])
		}
	}
	if got := dumpsJSON(chatChunk("c", 1, "m", pyval.Obj{{"content", "x"}}, "stop"), false); got != g["chat_chunk_plain"] {
		t.Errorf("chat.completion.chunk 序列化不同:\n go=%q\n py=%v", got, g["chat_chunk_plain"])
	}
	for _, row := range arr(g["want_usage"]) {
		r := row.([]any)
		if got := wantUsage(r[0]); got != r[1] {
			t.Errorf("_want_usage(%v) = %v，Python 给 %v", r[0], got, r[1])
		}
	}
}

func TestPaceTextGolden(t *testing.T) {
	g := loadAppFunc(t)
	for _, row := range arr(g["pace_text"]) {
		r := row.([]any)
		s, _ := r[0].(string)
		var got []string
		if err := paceEmit(s, 2, 12*time.Millisecond, func(x string) error {
			got = append(got, x)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !sameShapePy(stringsToAny(got), r[1]) {
			t.Errorf("_pace_text(%q) 分片不同: go=%v py=%v", s, got, r[1])
		}
	}
	for _, row := range arr(g["pace_text_chunked"]) {
		r := row.([]any)
		s, _ := r[0].(string)
		var got []string
		if err := paceEmit(s, 3, 12*time.Millisecond, func(x string) error {
			got = append(got, x)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !sameShapePy(stringsToAny(got), r[1]) {
			t.Errorf("_pace_text(%q,3,0.012) 分片不同: go=%v py=%v", s, got, r[1])
		}
	}
}

func TestPosAndMediaURLGolden(t *testing.T) {
	g := loadAppFunc(t)
	inputs := []any{nil, json.Number("0"), json.Number("1"), json.Number("-1"), "5", "abc",
		json.Number("1.9"), "", true}
	rows := arr(g["pos"])
	if len(rows) != len(inputs) {
		t.Fatalf("pos 用例数不同")
	}
	for i, in := range inputs {
		if got := pos(in); got != rows[i].([]any)[1] {
			t.Errorf("_pos(%v) = %v，Python 给 %v", in, got, rows[i].([]any)[1])
		}
	}
	a, _ := testApp(t, "k", time.Now())
	if got := a.mediaURL("x.webp"); got != arr(g["media_url"])[0] {
		t.Errorf("media_url = %q，Python 给 %v", got, arr(g["media_url"])[0])
	}
	a.CFG.PublicBase = "https://public.example/"
	if got := a.mediaURL("x.webp"); got != g["media_url_public"] {
		t.Errorf("media_url(public) = %q，Python 给 %v", got, g["media_url_public"])
	}
	if got := a.mediaURL("中文 名.webp"); got != g["media_url_public_slash"] {
		t.Errorf("media_url(中文名) = %q，Python 给 %v", got, g["media_url_public_slash"])
	}
}

func TestRequestDefaultsGolden(t *testing.T) {
	g := loadAppFunc(t)
	type pair struct {
		name string
		got  any
	}
	imgReq, ierrs := parseImageRequest(map[string]any{"prompt": "p"})
	vidReq, verrs := parseVideoRequest(map[string]any{"prompt": "p"})
	if len(ierrs) > 0 || len(verrs) > 0 {
		t.Fatalf("默认值解析不该报错: %v %v", ierrs, verrs)
	}
	cases := []pair{
		{"image_request_defaults", dumpImage(imgReq)},
		{"video_request_defaults", dumpVideo(vidReq)},
		{"chat_request_defaults", dumpChat(newChatRequest())},
		{"responses_request_defaults", dumpResponses(newResponsesRequest())},
	}
	for _, c := range cases {
		want, ok := g[c.name].(map[string]any)
		if !ok {
			t.Errorf("金样本里没有 %s", c.name)
			continue
		}
		if !sameShapePy(c.got, want) {
			t.Errorf("%s 默认值不同:\n go=%s\n py=%v", c.name, dumpsJSON(c.got, false), want)
		}
	}
	if g["models_const"] == nil {
		t.Error("金样本里没有 models_const")
	}
	if !sameShapePy(MODELS, g["models_const"]) {
		t.Errorf("MODELS 常量不同:\n go=%s\n py=%v", dumpsJSON(MODELS, false), g["models_const"])
	}

	// 路由层的超时兜底是 `req.timeout or CFG.xxx_timeout`：0 落默认值，负数原样传下去。
	// ImageRequest 那边 timeout 有 ge=1，0 早就 422 了，这条只对 video/chat 生效。
	for _, row := range arr(g["timeout_or"]) {
		r := row.([]any)
		var in *json.Number
		if n, ok := r[0].(json.Number); ok {
			in = &n
		}
		if got := pyNumOr(in, int64(numOr(r[1]))); got != int64(numOr(r[2])) {
			t.Errorf("timeout or default 不同: in=%v def=%v py=%v go=%d", r[0], r[1], r[2], got)
		}
	}
}

// --- 小工具 ---

// samePrompt：tool_calls 那两条用例把客户端原样的 JSON 结构再 dumps 进提示词，
// Go 的 map 无序 → 键序与 Python 的插入序不同（偏离清单第 13 条）。
// 只对这两条把内嵌 JSON 归一成排序后的写法再比，其余用例仍逐字节比。
func samePrompt(got any, want any, idx int) bool {
	if s, ok := want.(string); ok && (idx == 8 || idx == 9) {
		return canonEmbed(got.(string)) == canonEmbed(s)
	}
	return got == want
}

func canonEmbed(s string) string {
	m := embedRE.FindStringIndex(s)
	if m == nil {
		return s
	}
	dec := json.NewDecoder(strings.NewReader(s[m[0]:m[1]]))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	return s[:m[0]] + pyval.Dumps(v, false, true) + s[m[1]:]
}

func arr(v any) []any {
	l, _ := v.([]any)
	return l
}

func sortStrings(s []string) { sort.Strings(s) }

func stringsToAny(l []string) []any {
	out := make([]any, 0, len(l))
	for _, s := range l {
		out = append(out, s)
	}
	return out
}

// sameShapePy：把 Go 侧结构（含 pyval.Obj 与 struct）序列化成 JSON 再解析回来，
// 与金样本的解析结果做语义比较 —— 键序在这里不比，比的是键集合与值。
func sameShapePy(goVal any, py any) bool {
	b, err := json.Marshal(goVal)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var normalized any
	if err := dec.Decode(&normalized); err != nil {
		return false
	}
	return reflect.DeepEqual(normalizeNums(normalized), normalizeNums(py))
}

func normalizeNums(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalizeNums(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalizeNums(x)
		}
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	}
	return v
}
