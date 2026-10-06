package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"muse2api-go/internal/pyval"
)

// 工具调用（function calling）协议适配。muse.ai 的网页模型不返回结构化 tool_calls，
// 所以这里「注入协议 + 解析回来」两层。注意 config.tool_protocol 默认为 0，
// 注入那层默认不生效（实测 muse.ai 会明确拒绝输出伪工具调用），解析那层始终在跑。

const toolProtocolHead = "你可以根据需要调用以下工具来协助用户完成任务。\n" +
	"若需调用工具，请直接输出如下格式的 JSON 代码块（不要包含其他多余解释）：\n" +
	"```json\n{\"name\": \"<工具名>\", \"arguments\": {<参数>}}\n```\n" +
	"如果需要调用多个工具，请输出包含多个对象的 JSON 数组。\n" +
	"如果无需调用工具，请直接用自然语言回答。\n\n可用工具列表：\n"

// describeParams 对应 _describe_params()。
func describeParams(params any) string {
	m, ok := params.(map[string]any)
	if !ok {
		return "      （无参数）"
	}
	props, _ := m["properties"].(map[string]any)
	if len(props) == 0 {
		return "      （无参数）"
	}
	required := map[string]bool{}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	keys := sortedKeys(props)
	lines := make([]string, 0, len(keys))
	for _, name := range keys {
		spec, ok := props[name].(map[string]any)
		if !ok {
			spec = map[string]any{}
		}
		typ := "any"
		if v := orEmpty(spec["type"]); truthy(v) {
			typ = pyStr(v)
		}
		need := "可选"
		if required[name] {
			need = "必填"
		}
		tail := ""
		if d := orEmpty(spec["description"]); truthy(d) {
			tail = " " + pyStr(d)
		}
		lines = append(lines, "      - "+name+" ("+typ+", "+need+")"+tail)
	}
	return strings.Join(lines, "\n")
}

// buildToolsPrompt 对应 build_tools_prompt()，同时兼容 Chat Completions 的
// {"type":"function","function":{...}} 与 Responses API 的扁平形态。
func buildToolsPrompt(tools any) string {
	list, ok := tools.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	var items []string
	for _, t := range list {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn := tm
		if sub, ok := tm["function"].(map[string]any); ok {
			fn = sub
		}
		if !truthy(fn["name"]) {
			continue
		}
		desc := ""
		if d := orEmpty(fn["description"]); truthy(d) {
			desc = " — " + pyStr(d)
		}
		items = append(items, itoa(len(items)+1)+". "+pyStr(fn["name"])+desc+
			"\n   参数：\n"+describeParams(fn["parameters"]))
	}
	if len(items) == 0 {
		return ""
	}
	return toolProtocolHead + strings.Join(items, "\n") + "\n"
}

var fenceRE = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*(.*?)```")
var leadJSONRE = regexp.MustCompile("(?s)^(?:```)?(?:json|JSON)?\\s*")
var embedRE = regexp.MustCompile("(?s)(\\{.*\\}|\\[.*\\])")

// parseToolCalls 对应 parse_tool_calls()：先从最后一个代码块往前试，
// 再试裸 JSON，最后试文本里内嵌的完整 JSON。抽不到就返回原文。
func parseToolCalls(text string) ([]pyval.Obj, string) {
	if text == "" {
		return nil, text
	}
	all := fenceRE.FindAllStringSubmatchIndex(text, -1)
	for i := len(all) - 1; i >= 0; i-- {
		m := all[i]
		inner := strings.TrimSpace(text[m[2]:m[3]])
		calls := asToolCalls(jsonLoads(inner))
		if len(calls) > 0 {
			rest := strings.TrimSpace(text[:m[0]] + text[m[1]:])
			return calls, rest
		}
	}
	stripped := strings.TrimSpace(text)
	clean := strings.TrimSpace(strings.TrimRight(
		leadJSONRE.ReplaceAllString(stripped, ""), "`"))
	if strings.HasPrefix(clean, "{") || strings.HasPrefix(clean, "[") {
		if calls := asToolCalls(jsonLoads(clean)); len(calls) > 0 {
			return calls, ""
		}
	}
	if m := embedRE.FindStringSubmatchIndex(text); m != nil {
		calls := asToolCalls(jsonLoads(strings.TrimSpace(text[m[2]:m[3]])))
		if len(calls) > 0 {
			rest := strings.TrimSpace(text[:m[0]] + text[m[1]:])
			// 剩下的如果只是 ```json 那层围栏标记，等价于「正文已被吃掉」。
			if strings.TrimSpace(strings.TrimRight(leadJSONRE.ReplaceAllString(rest, ""), "`")) == "" {
				rest = ""
			}
			return calls, rest
		}
	}
	return nil, text
}

// asToolCalls 对应 _as_tool_calls()：不像工具调用就返回空。
func asToolCalls(obj any) []pyval.Obj {
	var raw []any
	switch v := obj.(type) {
	case []any:
		raw = v
	case nil:
		return nil
	default:
		raw = []any{v}
	}
	if len(raw) == 0 {
		return nil
	}
	var out []pyval.Obj
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		name := m["tool"]
		if !truthy(name) {
			name = m["name"]
		}
		if !truthy(name) {
			name = m["function"]
		}
		s, ok := name.(string)
		if !ok || s == "" {
			return nil
		}
		args := m["arguments"]
		if args == nil {
			args = orEmpty(m["parameters"])
			if !truthy(args) {
				args = orEmpty(m["args"])
			}
			if !truthy(args) {
				args = map[string]any{}
			}
		}
		argStr, isStr := args.(string)
		if !isStr {
			argStr = pyval.Dumps(args, false, false)
		}
		out = append(out, pyval.Obj{
			{"id", "call_" + uuidHex(20)},
			{"type", "function"},
			{"function", pyval.Obj{{"name", s}, {"arguments", argStr}}},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func jsonLoads(s string) any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	// Python 的 json.loads 不接受尾随内容，Go 的 Decoder 会只看第一个值。
	rest := strings.TrimSpace(s[dec.InputOffset():])
	if rest != "" {
		return nil
	}
	return v
}

func uuidHex(n int) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)[:n]
}

var idRe = regexp.MustCompile("call_[0-9a-f]{20}")
