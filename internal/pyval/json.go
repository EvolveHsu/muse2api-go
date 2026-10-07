package pyval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 这一档是 Python 的 json.dumps 语义，不是 Go 的 encoding/json：
// 默认分隔符是 ", " 与 ": "；ensure_ascii 决定非 ASCII 是否转 \uXXXX；
// '/' 不转义；map 侧一律排序。
// SSE 帧、幂等哈希、工具调用 arguments 按 json.dumps 默认写法落字节；
// HTTP 响应走 FastAPI 的 JSONResponse，它是 separators=(",", ":") —— 两种都要有。

// Pair 是有序对象的键值对。
type Pair struct {
	Key string
	Val any
}

// Obj 保键序 —— Go 的 map 无序，而 Python dict 保插入序；
// 响应体里键序有语义（面板与客户端按顺序渲染/取用）时用它。
type Obj []Pair

// Dumps 对应 json.dumps(obj, ensure_ascii=..., sort_keys=...)，不带 indent。
func Dumps(v any, ensureASCII, sortKeys bool) string {
	var b strings.Builder
	writeJSON(&b, v, ensureASCII, ", ", ": ")
	return b.String()
}

// MarshalCompact 对应 json.dumps(..., separators=(",", ":"))，即 FastAPI 的响应写法。
func MarshalCompact(v any) string {
	var b strings.Builder
	writeJSON(&b, v, false, ",", ":")
	return b.String()
}

// MarshalJSON 让 Obj 能进 encoding/json 的通路，同时保住键序与 Python 的转义规则。
func (o Obj) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	writeJSON(&b, o, false, ",", ":")
	return []byte(b.String()), nil
}

func writeJSON(b *strings.Builder, v any, ascii bool, itemSep, kvSep string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case Obj:
		b.WriteByte('{')
		for i, p := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeString(b, p.Key, ascii)
			b.WriteString(kvSep)
			writeJSON(b, p.Val, ascii, itemSep, kvSep)
		}
		b.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Python dict 保插入序，Go map 没有序，这里一律排序：
		// 只有 sort_keys=True 的调用（幂等哈希）因此能与 Python 逐字节一致，
		// 其余有序输出必须显式用 Obj。键序这一类差异不编号（见 README 差异一节开头的说明）：
		// 两侧读方都按键取值，不影响对外行为。
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeString(b, k, ascii)
			b.WriteString(kvSep)
			writeJSON(b, t[k], ascii, itemSep, kvSep)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeJSON(b, x, ascii, itemSep, kvSep)
		}
		b.WriteByte(']')
	case string:
		writeString(b, t, ascii)
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(t.String())
	case float64:
		b.WriteString(Float(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	default:
		// 其余 Go 类型（自定义 struct、[]map[string]any、[]string 等）交给 encoding/json：
		// 键序按字母排，但值不会像 fmt.Sprintf 那样被糊成字符串。
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			b.WriteString(strconv.Quote(fmt.Sprintf("%v", v)))
			return
		}
		b.WriteString(strings.TrimRight(buf.String(), "\n"))
	}
}

func writeString(b *strings.Builder, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case !ascii:
				// ensure_ascii=False：非 ASCII 原样写出（UTF-8）。
				b.WriteRune(r)
			case r <= 0x7E:
				// ensure_ascii=True 的判据是 CPython 的 ESCAPE_ASCII = ([\\"]|[^\ -~])：
				// 「空格 ~ 波浪号」之间的一律原样，之外的才转义 —— 所以 0x7F（DEL）也要转。
				b.WriteRune(r)
			case r <= 0xFFFF:
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				// ensure_ascii=True：增补平面字符拆成一对代理项。
				r1 := 0xD800 + ((r - 0x10000) >> 10)
				r2 := 0xDC00 + ((r - 0x10000) & 0x3FF)
				fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
			}
		}
	}
	b.WriteByte('"')
}
