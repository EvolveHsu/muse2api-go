package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"muse2api-go/internal/pyval"
)

// decodeBody 用 json.Number 承载数字：Python 的 1（int）与 1.0（float）在
// str()、json.dumps()、幂等哈希里写法不同，一旦过成 Go float64 就分不出来了。
// 坏 JSON 在这里返回普通 error —— 对应 Python 里 request.json() 抛 JSONDecodeError
// 且不被捕获，最终由 Starlette 的 ServerErrorMiddleware 回 500 "Internal Server Error"。
func decodeBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if rest := strings.TrimSpace(string(raw)[dec.InputOffset():]); rest != "" {
		return nil, &json.SyntaxError{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nilValueError{}
	}
	return m, nil
}

type nilValueError struct{}

func (nilValueError) Error() string { return "body is not an object" }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func itoa(i int) string { return strconv.Itoa(i) }

// vErr 是一条 pydantic 风格的校验错误。loc 是元组（不是数组），
// 渲染成 Python 的 repr 时括号和逗号都要照抄。
type vErr struct {
	Type  string
	Loc   []any
	Msg   string
	Input any
	Ctx   map[string]any
}

func (e vErr) repr() string {
	parts := []string{"'type': " + pyval.Repr(e.Type), "'loc': " + tupleRepr(e.Loc),
		"'msg': " + pyval.Repr(e.Msg), "'input': " + errInputRepr(e.Input)}
	if len(e.Ctx) > 0 {
		parts = append(parts, "'ctx': "+pyval.Repr(e.Ctx))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// detailEntry 是 /admin 那边给前端的 JSON 形态（FastAPI 用 json.dumps，
// loc 元组降级成数组，键序就是 pydantic 的插入序）。
type detailEntry struct {
	Type  string         `json:"type"`
	Loc   []any          `json:"loc"`
	Msg   string         `json:"msg"`
	Input any            `json:"input"`
	Ctx   map[string]any `json:"ctx,omitempty"`
}

func (e vErr) detail() detailEntry {
	return detailEntry{Type: e.Type, Loc: e.Loc, Msg: e.Msg, Input: e.Input, Ctx: e.Ctx}
}

// errInputRepr：被拒的输入原样用 Python repr 打出来，但 None 缺字段时是 'None'。
func errInputRepr(v any) string {
	if v == nil {
		return "None"
	}
	return pyval.Repr(v)
}

func tupleRepr(items []any) string {
	parts := make([]string, 0, len(items))
	for _, x := range items {
		parts = append(parts, pyval.Repr(x))
	}
	if len(parts) == 1 {
		return "(" + parts[0] + ",)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// validationRepr 对应 str(exc.errors())[:400]：整份列表的 repr 再按字符数截断。
func validationRepr(errs []vErr) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.repr())
	}
	return truncate("["+strings.Join(parts, ", ")+"]", 400)
}
