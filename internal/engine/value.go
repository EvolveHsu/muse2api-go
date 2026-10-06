package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Value 是 CDP Runtime.evaluate 回传值的承载类型。
//
// 为什么不用 map[string]any：Python 里 `page.js(...) or ""`、`if not raw`、
// `int(raw or 0)` 都依赖「空字符串/0/false/null 一律为假」这条语言规则；
// 而 text/template 会把 int 0、false、空切片当成"空"来填 <no value>。
// 自己实现 truthy 与 MarshalJSON，既保住 Python 语义，也保住 JSON 数字不被
// float64 往返改写（1e999 这类值一旦被 Go 序列化成 +Inf.0，上游就崩了）。
type Value struct {
	Kind  ValueKind
	Str   string
	Bits  string // 数字的原始 JSON 文本，保持与 Python 回传一致
	Num   float64
	Bool  bool
	Obj   map[string]Value
	Array []Value
}

type ValueKind byte

const (
	KNull ValueKind = iota
	KBool
	KNumber
	KString
	KObject
	KArray
)

func Null() Value                     { return Value{Kind: KNull} }
func BoolV(b bool) Value              { return Value{Kind: KBool, Bool: b} }
func StrV(s string) Value             { return Value{Kind: KString, Str: s} }
func NumF(f float64) Value            { return Value{Kind: KNumber, Num: f, Bits: jsonNumber(f)} }
func Object(m map[string]Value) Value { return Value{Kind: KObject, Obj: m} }

// jsonNumber 复刻 Python json.dumps 对浮点的处理：整数值输出成 int。
func jsonNumber(f float64) string {
	if math.IsInf(f, 1) {
		return "1e999"
	}
	if math.IsInf(f, -1) {
		return "-1e999"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Truthy 对应 Python 的真假判断。
func (v Value) Truthy() bool {
	switch v.Kind {
	case KNull:
		return false
	case KBool:
		return v.Bool
	case KNumber:
		return v.Num != 0
	case KString:
		return v.Str != ""
	case KObject:
		return len(v.Obj) > 0
	case KArray:
		return len(v.Array) > 0
	}
	return false
}

// Str 取字符串；数字按 Python str() 的形式给出，null 给空串（对应 `or ""`）。
func (v Value) Str2() string {
	switch v.Kind {
	case KString:
		return v.Str
	case KNull:
		return ""
	case KBool:
		if v.Bool {
			return "True"
		}
		return "False"
	case KNumber:
		return v.display()
	}
	return ""
}

func (v Value) display() string {
	if v.Bits != "" {
		return v.Bits
	}
	return jsonNumber(v.Num)
}

// Int 对应 int(x or 0)。
func (v Value) Int() int {
	switch v.Kind {
	case KNumber:
		return int(v.Num)
	case KString:
		n, err := strconv.Atoi(v.Str)
		if err != nil {
			return 0
		}
		return n
	case KBool:
		if v.Bool {
			return 1
		}
	}
	return 0
}

func (v Value) Get(key string) Value {
	if v.Kind == KObject {
		if x, ok := v.Obj[key]; ok {
			return x
		}
	}
	return Null()
}

func (v Value) Len() int {
	switch v.Kind {
	case KString:
		return len([]rune(v.Str))
	case KArray:
		return len(v.Array)
	case KObject:
		return len(v.Obj)
	}
	return 0
}

func (v Value) Index(i int) Value {
	if v.Kind == KArray && i >= 0 && i < len(v.Array) {
		return v.Array[i]
	}
	return Null()
}

func (v Value) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case KNull:
		return []byte("null"), nil
	case KBool:
		if v.Bool {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case KNumber:
		s := v.display()
		if s == "NaN" {
			return nil, fmt.Errorf("JSON 里不允许 NaN")
		}
		return []byte(s), nil
	case KString:
		return json.Marshal(v.Str)
	case KObject:
		var b bytes.Buffer
		b.WriteByte('{')
		first := true
		for _, k := range sortedKeys(v.Obj) {
			if !first {
				b.WriteByte(',')
			}
			first = false
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			vb, err := json.Marshal(v.Obj[k])
			if err != nil {
				return nil, err
			}
			b.Write(vb)
		}
		b.WriteByte('}')
		return b.Bytes(), nil
	case KArray:
		return json.Marshal(v.Array)
	}
	return []byte("null"), nil
}

func sortedKeys(m map[string]Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// parseValue 解析 CDP 回传的 JSON；数字保留原始文本。
func parseValue(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return decodeToken(dec)
}

func decodeToken(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Null(), err
	}
	return decodeValue(dec, tok)
}

func decodeValue(dec *json.Decoder, tok json.Token) (Value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]Value{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return Null(), err
				}
				key, _ := kt.(string)
				vt, err := dec.Token()
				if err != nil {
					return Null(), err
				}
				val, err := decodeValue(dec, vt)
				if err != nil {
					return Null(), err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return Null(), err
			}
			return Value{Kind: KObject, Obj: obj}, nil
		case '[':
			arr := []Value{}
			for dec.More() {
				vt, err := dec.Token()
				if err != nil {
					return Null(), err
				}
				val, err := decodeValue(dec, vt)
				if err != nil {
					return Null(), err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return Null(), err
			}
			return Value{Kind: KArray, Array: arr}, nil
		}
		return Null(), fmt.Errorf("意外的分隔符 %v", t)
	case json.Number:
		f, _ := strconv.ParseFloat(t.String(), 64)
		return Value{Kind: KNumber, Num: f, Bits: t.String()}, nil
	case string:
		return StrV(t), nil
	case bool:
		return BoolV(t), nil
	case nil:
		return Null(), nil
	}
	return Null(), fmt.Errorf("不支持的 JSON 类型 %T", tok)
}
