package pyval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Str 复刻 Python 的 str(value)：字符串原样，None/True/False 用 Python 拼写，
// 容器递归走 Repr。请求体里的值都按 json.Number 传进来（见 server.decodeBody），
// 所以 Python 的 1（int）与 1.0（float）不会被并成一个 Go float64 而写错字面量。
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Repr(v)
}

// Repr 复刻 Python 的 repr(value)（对字符串就是带单引号那层，用于 str(dict) 的场景）。
func Repr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pyReprString(t)
	case json.Number:
		return t.String()
	case float64:
		return Float(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, Repr(x))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Python dict 保插入序，Go map 无序；这条路径只出现在把容器值 str() 进文本里
		// （_content_text 收到 dict 之类的病态输入），排序只为结果稳定。
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, pyReprString(k)+": "+Repr(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return Str2(v)
}

// Str2 是最后一层兜底：不认识的 Go 类型（理论上到不了）退化成 Go 自己的写法。
func Str2(v any) string { return fmt.Sprintf("%v", v) }

func pyReprString(s string) string {
	q := strings.ReplaceAll(s, `\`, `\\`)
	q = strings.ReplaceAll(q, `'`, `\'`)
	if strings.Contains(q, "\n") || strings.Contains(q, "\t") || strings.Contains(q, "\r") {
		// Python 的 repr 会把控制字符写成转义序列。
		q = strings.ReplaceAll(q, "\n", `\n`)
		q = strings.ReplaceAll(q, "\t", `\t`)
		q = strings.ReplaceAll(q, "\r", `\r`)
	}
	return "'" + q + "'"
}
