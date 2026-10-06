package pyval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
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

// pyReprString 是 Python 的 repr(str)。三条容易漏的规则：
//   - 引号默认单引号，但**内容里有单引号且没有双引号**时改用双引号
//     （`Expecting ':' delimiter` 这类带引号的英文文案正走这条分支，gen_badjson_0 钉着它）；
//   - \t \n \r 用具名转义，其余 <0x20 与 0x7F 用 \xNN；
//   - 不可打印的非 ASCII 用 \uXXXX，增补平面用 \UXXXXXXXX。
func pyReprString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var sb strings.Builder
	sb.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, `\x%02x`, r)
		case r >= 0x80 && !unicode.IsPrint(r):
			if r <= 0xFFFF {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				fmt.Fprintf(&sb, `\U%08x`, r)
			}
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte(quote)
	return sb.String()
}
