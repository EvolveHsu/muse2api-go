package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"muse2api-go/internal/pyval"
)

// 这四个 parse* 对应 app.py 的四个 pydantic 模型。
// 逐字段校验的**顺序**就是 Python 的字段声明顺序 —— pydantic 是按声明顺序收集错误的，
// 顺序错了客户端看到的 detail 列表就与 Python 不同（金样本里 6 条 422 就是照这个对的）。

var bodyLoc = []any{"body"}

// wantAny 对应 pydantic 的 `x: Any` —— 收任何值，缺省给 nil。
func wantAny(body map[string]any, field string) any {
	if v, ok := body[field]; ok {
		return v
	}
	return nil
}

func (a *App) validationFailed(r *http.Request, errs []vErr) error {
	return validationErrorFor(r.URL.Path, errs)
}

// requiredStr：类型是 str 且没默认值 —— 缺字段算 missing，传 null 算 string_type。
func requiredStr(body map[string]any, field string, errs *[]vErr) (string, bool) {
	raw, ok := body[field]
	if !ok {
		*errs = append(*errs, vErr{Type: "missing", Loc: append(append([]any{}, bodyLoc...), field),
			Msg: "Field required", Input: body})
		return "", false
	}
	if s, isStr := raw.(string); isStr {
		return s, true
	}
	*errs = append(*errs, vErr{Type: "string_type", Loc: append(append([]any{}, bodyLoc...), field),
		Msg: "Input should be a valid string", Input: raw})
	return "", false
}

// strWithDefault：`x: str = "v"` —— null 也是类型错误。
func strWithDefault(body map[string]any, field, def string, errs *[]vErr) string {
	raw, ok := body[field]
	if !ok {
		return def
	}
	if s, isStr := raw.(string); isStr {
		return s
	}
	*errs = append(*errs, vErr{Type: "string_type", Loc: append(append([]any{}, bodyLoc...), field),
		Msg: "Input should be a valid string", Input: raw})
	return def
}

// strOrNull：`x: str | None = None`。
func strOrNull(body map[string]any, field string, errs *[]vErr) *string {
	raw, ok := body[field]
	if !ok || raw == nil {
		return nil
	}
	if s, isStr := raw.(string); isStr {
		return &s
	}
	*errs = append(*errs, vErr{Type: "string_type", Loc: append(append([]any{}, bodyLoc...), field),
		Msg: "Input should be a valid string", Input: raw})
	return nil
}

func intOrNull(body map[string]any, field string, errs *[]vErr, ge, le *int64) *json.Number {
	raw, ok := body[field]
	if !ok || raw == nil {
		return nil
	}
	loc := append(append([]any{}, bodyLoc...), field)
	n := coerceInt(raw)
	if n == nil {
		*errs = append(*errs, vErr{Type: "int_type", Loc: loc,
			Msg: "Input should be a valid integer", Input: raw})
		return nil
	}
	v, _ := n.Int64()
	if le != nil && v > *le {
		*errs = append(*errs, vErr{Type: "less_than_equal", Loc: loc,
			Msg:   "Input should be less than or equal to " + itoa64(*le),
			Input: raw, Ctx: map[string]any{"le": *le}})
		return nil
	}
	if ge != nil && v < *ge {
		*errs = append(*errs, vErr{Type: "greater_than_equal", Loc: loc,
			Msg:   "Input should be greater than or equal to " + itoa64(*ge),
			Input: raw, Ctx: map[string]any{"ge": *ge}})
		return nil
	}
	return n
}

func itoa64(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{digits[v%10]}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func coerceInt(raw any) *json.Number {
	switch v := raw.(type) {
	case json.Number:
		if strings.ContainsAny(v.String(), ".eE") {
			f, err := v.Float64()
			if err != nil || f != float64(int64(f)) {
				return nil
			}
			n := json.Number(itoa64(int64(f)))
			return &n
		}
		n := v
		return &n
	case bool:
		return nil
	case string:
		n := json.Number(v)
		if i, err := parseIntStrict(v); err == nil {
			n = json.Number(itoa64(i))
			return &n
		}
		return nil
	}
	return nil
}

func parseIntStrict(s string) (int64, error) {
	var v int64
	neg := false
	body := s
	if strings.HasPrefix(body, "-") {
		neg, body = true, body[1:]
	} else {
		body = strings.TrimPrefix(body, "+")
	}
	if body == "" {
		return 0, errNum
	}
	for _, r := range body {
		if r < '0' || r > '9' {
			return 0, errNum
		}
		v = v*10 + int64(r-'0')
	}
	if neg {
		v = -v
	}
	return v, nil
}

var errNum = &numError{}

type numError struct{}

func (*numError) Error() string { return "invalid number" }

func boolWithDefault(body map[string]any, field string, def bool, errs *[]vErr) bool {
	raw, ok := body[field]
	if !ok || raw == nil {
		if raw == nil && ok {
			*errs = append(*errs, vErr{Type: "bool_type",
				Loc: append(append([]any{}, bodyLoc...), field),
				Msg: "Input should be a valid boolean", Input: raw})
			return def
		}
		return def
	}
	if b, isB := raw.(bool); isB {
		return b
	}
	if s, isS := raw.(string); isS {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "on", "yes", "t", "y":
			return true
		case "false", "0", "off", "no", "f", "n":
			return false
		}
	}
	*errs = append(*errs, vErr{Type: "bool_type",
		Loc: append(append([]any{}, bodyLoc...), field),
		Msg: "Input should be a valid boolean", Input: raw})
	return def
}

func listOfAny(body map[string]any, field string, nullable bool, errs *[]vErr) []any {
	raw, ok := body[field]
	if !ok || raw == nil {
		if !ok || nullable {
			return nil
		}
		*errs = append(*errs, vErr{Type: "missing",
			Loc: append(append([]any{}, bodyLoc...), field),
			Msg: "Field required", Input: body})
		return nil
	}
	if v, isL := raw.([]any); isL {
		return v
	}
	*errs = append(*errs, vErr{Type: "list_type",
		Loc: append(append([]any{}, bodyLoc...), field),
		Msg: "Input should be a valid list", Input: raw})
	return nil
}

// --- ImageRequest ---

func parseImageRequest(body map[string]any) (*ImageRequest, []vErr) {
	var errs []vErr
	req := newImageRequest()
	req.Prompt, _ = requiredStr(body, "prompt", &errs)
	req.Model = strWithDefault(body, "model", "muse-image", &errs)
	if n := intOrNull(body, "n", &errs, nil, nil); n != nil {
		req.N = *n
	}
	req.Size = strOrNull(body, "size", &errs)
	req.AspectRatio = strOrNull(body, "aspect_ratio", &errs)
	req.ResponseFormat = strWithDefault(body, "response_format", "url", &errs)
	req.Timeout = intOrNull(body, "timeout", &errs, ptr64(1), ptr64(600))
	req.Extra = strOrNull(body, "extra", &errs)
	req.Image = wantAny(body, "image")
	req.Images = listOfAny(body, "images", true, &errs)
	req.Reference = strOrNull(body, "reference_image", &errs)
	// async 是别名字段：Python 用 Field(False, alias="async")，对外只认 "async"。
	req.Async = boolWithDefault(body, "async", false, &errs)
	return req, errs
}

func ptr64(v int64) *int64 { return &v }

// --- VideoRequest ---

func parseVideoRequest(body map[string]any) (*VideoRequest, []vErr) {
	var errs []vErr
	req := newVideoRequest()
	req.Prompt, _ = requiredStr(body, "prompt", &errs)
	req.Model = strWithDefault(body, "model", "muse-video", &errs)
	req.Duration = intOrNull(body, "duration", &errs, nil, nil)
	req.Size = strOrNull(body, "size", &errs)
	req.AspectRatio = strOrNull(body, "aspect_ratio", &errs)
	req.Resolution = strOrNull(body, "resolution", &errs)
	req.Timeout = intOrNull(body, "timeout", &errs, nil, nil)
	req.Extra = strOrNull(body, "extra", &errs)
	req.Image = wantAny(body, "image")
	req.ImageURL = wantAny(body, "image_url")
	req.Reference = strOrNull(body, "reference_image", &errs)
	return req, errs
}

// --- ChatRequest ---

func parseChatRequest(body map[string]any) (*ChatRequest, []vErr) {
	var errs []vErr
	req := newChatRequest()
	req.Model = strWithDefault(body, "model", "muse-spark", &errs)
	req.Messages = parseMessages(body, &errs)
	req.Stream = boolWithDefault(body, "stream", false, &errs)
	req.Temperature = intOrNull(body, "temperature", &errs, nil, nil)
	req.MaxTokens = intOrNull(body, "max_tokens", &errs, nil, nil)
	req.Timeout = intOrNull(body, "timeout", &errs, nil, nil)
	req.Prompt = strOrNull(body, "prompt", &errs)
	req.Tools = wantAny(body, "tools")
	req.ToolChoice = wantAny(body, "tool_choice")
	req.ResponseFormat = wantAny(body, "response_format")
	req.StreamOptions = wantAny(body, "stream_options")
	return req, errs
}

// parseMessages：messages 是 list[ChatMessage]，非 Optional —— 传 null 也是类型错。
func parseMessages(body map[string]any, errs *[]vErr) []ChatMessage {
	raw, ok := body["messages"]
	loc := append(append([]any{}, bodyLoc...), "messages")
	if !ok {
		return nil
	}
	if raw == nil {
		*errs = append(*errs, vErr{Type: "list_type", Loc: loc,
			Msg: "Input should be a valid list", Input: raw})
		return nil
	}
	list, isList := raw.([]any)
	if !isList {
		*errs = append(*errs, vErr{Type: "list_type", Loc: loc,
			Msg: "Input should be a valid list", Input: raw})
		return nil
	}
	out := make([]ChatMessage, 0, len(list))
	for i, item := range list {
		m, isMap := item.(map[string]any)
		if !isMap {
			*errs = append(*errs, vErr{Type: "model_attributes_type", Loc: append(loc, i),
				Msg: "Input should be a valid dictionary or object to extract fields from", Input: item})
			continue
		}
		var subErrs []vErr
		role, _ := requiredStrIn(m, "role", []any{"body", "messages", i}, &subErrs)
		cm := ChatMessage{Role: role}
		cm.Content = messageContent(m, []any{"body", "messages", i}, &subErrs)
		cm.Name = strOrNullIn(m, "name", []any{"body", "messages", i}, &subErrs)
		cm.ToolCall = strOrNullIn(m, "tool_call_id", []any{"body", "messages", i}, &subErrs)
		cm.ToolCalls = wantAny(m, "tool_calls")
		out = append(out, cm)
		*errs = append(*errs, subErrs...)
	}
	return out
}

// messageContent：content 是 str | list | None。
func messageContent(m map[string]any, loc []any, errs *[]vErr) any {
	raw, ok := m["content"]
	if !ok || raw == nil {
		return nil
	}
	switch raw.(type) {
	case string, []any:
		return raw
	}
	// union 两个分支都不匹配：pydantic 会给两条错误
	*errs = append(*errs,
		vErr{Type: "string_type", Loc: append(append([]any{}, loc...), "str"),
			Msg: "Input should be a valid string", Input: raw},
		vErr{Type: "list_type", Loc: append(append([]any{}, loc...), "list[Any]"),
			Msg: "Input should be a valid list", Input: raw})
	return nil
}

func requiredStrIn(m map[string]any, field string, loc []any, errs *[]vErr) (string, bool) {
	raw, ok := m[field]
	if !ok {
		*errs = append(*errs, vErr{Type: "missing", Loc: append(append([]any{}, loc...), field),
			Msg: "Field required", Input: m})
		return "", false
	}
	if s, isStr := raw.(string); isStr {
		return s, true
	}
	*errs = append(*errs, vErr{Type: "string_type", Loc: append(append([]any{}, loc...), field),
		Msg: "Input should be a valid string", Input: raw})
	return "", false
}

func strOrNullIn(m map[string]any, field string, loc []any, errs *[]vErr) *string {
	raw, ok := m[field]
	if !ok || raw == nil {
		return nil
	}
	if s, isStr := raw.(string); isStr {
		return &s
	}
	*errs = append(*errs, vErr{Type: "string_type", Loc: append(append([]any{}, loc...), field),
		Msg: "Input should be a valid string", Input: raw})
	return nil
}

// --- ResponsesRequest ---

func parseResponsesRequest(body map[string]any) (*ResponsesRequest, []vErr) {
	var errs []vErr
	req := newResponsesRequest()
	req.Model = strWithDefault(body, "model", "muse-spark", &errs)
	if raw, ok := body["input"]; ok && raw != nil {
		switch raw.(type) {
		case string, []any:
			req.Input = raw
		default:
			// union：两个分支都不匹配时报两条，loc 尾巴是分支名（金样本原样记着）。
			errs = append(errs,
				vErr{Type: "string_type", Loc: []any{"body", "input", "str"},
					Msg: "Input should be a valid string", Input: raw},
				vErr{Type: "list_type", Loc: []any{"body", "input", "list[any]"},
					Msg: "Input should be a valid list", Input: raw})
		}
	}
	req.Instructions = strOrNull(body, "instructions", &errs)
	req.Stream = boolWithDefault(body, "stream", false, &errs)
	req.MaxOutput = intOrNull(body, "max_output_tokens", &errs, nil, nil)
	req.Timeout = intOrNull(body, "timeout", &errs, nil, nil)
	req.Tools = wantAny(body, "tools")
	if v, ok := body["store"]; ok && v != nil {
		if b, isB := v.(bool); isB {
			req.Store = &b
		} else {
			errs = append(errs, vErr{Type: "bool_type", Loc: []any{"body", "store"},
				Msg: "Input should be a valid boolean", Input: v})
		}
	}
	return req, errs
}

// pos 对应 _pos()：能 int() 且 > 0 才算数（Python 的 int("5") 可以，int("abc") 抛错）。
func pos(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		if t {
			return true
		}
		return false
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i > 0
		}
		f, err := t.Float64()
		return err == nil && int64(f) > 0
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return err == nil && i > 0
	case float64:
		return int64(t) > 0
	case int:
		return t > 0
	}
	return false
}

// dumpImage/dumpVideo/dumpChat/dumpResponses 对应 pydantic 的 model_dump：
// 键序 = 字段声明顺序，幂等哈希与任务落盘都会用到它。
func dumpImage(r *ImageRequest) pyval.Obj {
	return pyval.Obj{
		{"prompt", r.Prompt}, {"model", r.Model}, {"n", numOrNull(r.N)},
		{"size", ptrVal(r.Size)}, {"aspect_ratio", ptrVal(r.AspectRatio)},
		{"response_format", r.ResponseFormat}, {"timeout", numPtr(r.Timeout)},
		{"extra", ptrVal(r.Extra)}, {"image", r.Image}, {"images", anySlice(r.Images)},
		{"reference_image", ptrVal(r.Reference)}, {"async", r.Async},
	}
}

func dumpVideo(r *VideoRequest) pyval.Obj {
	return pyval.Obj{
		{"prompt", r.Prompt}, {"model", r.Model}, {"duration", numPtr(r.Duration)},
		{"size", ptrVal(r.Size)}, {"aspect_ratio", ptrVal(r.AspectRatio)},
		{"resolution", ptrVal(r.Resolution)}, {"timeout", numPtr(r.Timeout)},
		{"extra", ptrVal(r.Extra)}, {"image", r.Image}, {"image_url", r.ImageURL},
		{"reference_image", ptrVal(r.Reference)},
	}
}

func dumpChat(r *ChatRequest) pyval.Obj {
	msgs := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, dumpMessage(m))
	}
	return pyval.Obj{
		{"model", r.Model}, {"messages", msgs}, {"stream", r.Stream},
		{"temperature", numPtr(r.Temperature)}, {"max_tokens", numPtr(r.MaxTokens)},
		{"timeout", numPtr(r.Timeout)}, {"prompt", ptrVal(r.Prompt)},
		{"tools", r.Tools}, {"tool_choice", r.ToolChoice},
		{"response_format", r.ResponseFormat}, {"stream_options", r.StreamOptions},
	}
}

func dumpMessage(m ChatMessage) pyval.Obj {
	return pyval.Obj{
		{"role", m.Role}, {"content", m.Content}, {"name", ptrVal(m.Name)},
		{"tool_call_id", ptrVal(m.ToolCall)}, {"tool_calls", m.ToolCalls},
	}
}

func dumpResponses(r *ResponsesRequest) pyval.Obj {
	return pyval.Obj{
		{"model", r.Model}, {"input", r.Input}, {"instructions", ptrVal(r.Instructions)},
		{"stream", r.Stream}, {"max_output_tokens", numPtr(r.MaxOutput)},
		{"timeout", numPtr(r.Timeout)}, {"tools", r.Tools}, {"store", boolPtr(r.Store)},
	}
}

func ptrVal(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func boolPtr(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func numOrNull(n json.Number) any {
	if n.String() == "" {
		return nil
	}
	return n
}

func numPtr(n *json.Number) any {
	if n == nil {
		return nil
	}
	return *n
}

func anySlice(l []any) any {
	if l == nil {
		return nil
	}
	return l
}
