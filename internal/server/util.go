package server

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"muse2api-go/internal/engine"

	"muse2api-go/internal/pyval"
)

// strOf / numOr 这些是「从 store 里的 map[string]any 取值」的惯用写法。
// store 的 map 是从 JSON 读的，数字一律 json.Number（见 decodeBody 的理由）。

func strOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	}
	return pyStr(v)
}

func numOr(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return 0
}

func orDefaultNum(v any, def int) int {
	if v == nil {
		return def
	}
	if n, ok := v.(json.Number); ok && n.String() == "" {
		return def
	}
	if v == nil {
		return def
	}
	return int(numOr(v))
}

func numOrPtr(n *json.Number, def int64) int64 {
	if n == nil {
		return def
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, err := n.Float64()
	if err != nil {
		return def
	}
	return int64(f)
}

func toStringMap(v any) map[string]string {
	out := map[string]string{}
	m, ok := v.(map[string]any)
	if !ok {
		if already, ok2 := v.(map[string]string); ok2 {
			return already
		}
		return out
	}
	for k, x := range m {
		out[k] = strOf(x)
	}
	return out
}

func toIntMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// cookieIntMap 对应把 cookies_exp 交给 engine 前的转换：非数字值退回 0（Python 端 float() 会抛，
// 但线上的 cookies_exp 全是我们自己写的时间戳）。
func cookieIntMap(v any) map[string]int {
	out := map[string]int{}
	m, ok := v.(map[string]any)
	if !ok {
		if already, ok2 := v.(map[string]int); ok2 {
			return already
		}
		return out
	}
	for k, x := range m {
		switch t := x.(type) {
		case json.Number:
			if i, err := t.Int64(); err == nil {
				out[k] = int(i)
				continue
			}
			if f, err := t.Float64(); err == nil {
				out[k] = int(f)
			}
		case float64:
			out[k] = int(t)
		case int:
			out[k] = t
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				out[k] = int(f)
			}
		}
	}
	return out
}

func cookieMap(v any) map[string]string { return toStringMap(v) }

func boolPtrT(b bool) *bool { return &b }

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func round1(v float64) float64 {
	// Python 的 round(x, 1) 是银行家舍入 + 最短表示；这里只需要与它落盘结果一致，
	// 用 strconv 的十进制写法避免二进制噪声。
	f := math.Round(v*10) / 10
	return f
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func maxF(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

func nowUnixStr(t time.Time) int64 { return t.Unix() }

var _ = pyval.MarshalCompact

// optStr：空串当 None（Python 侧这些字段默认 None，区分「没给」与「给了空」）。
func optStr(v any) *string {
	s := strOf(v)
	if s == "" {
		return nil
	}
	return &s
}

// fv 取 multipart 表单值（net/http 给的是 map[string][]string，Starlette 的 form.get 拿第一个）。
func fv(f map[string][]string, key string) string {
	if vs := f[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func parseInt(s string) (json.Number, error) {
	n := json.Number(strings.TrimSpace(s))
	if _, err := n.Int64(); err != nil {
		return "", err
	}
	return n, nil
}

// cloneMap 对应 dict(task)：浅拷贝一份再改，绝不能动 store 里那个活对象。
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func anySliceOf(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// resultFromMap：异步任务落盘的是 {filename,size,kind,url}，
// GET 任务时要把这个壳还原成 _image_response 认得的结构。
func resultFromMap(v any) *engine.GenResult {
	m, ok := v.(map[string]any)
	if !ok {
		return &engine.GenResult{}
	}
	return &engine.GenResult{
		Filename: strOf(m["filename"]),
		Size:     int64(numOr(m["size"])),
		Kind:     strOf(m["kind"]),
		Path:     strOf(m["path"]),
	}
}

func orDefault(v any, def string) string {
	s := strOf(v)
	if s == "" {
		return def
	}
	return s
}
