package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"muse2api-go/internal/pyval"
)

// pyBytes 表示「Content-Type 不是 JSON，于是 body 保持 bytes」那个状态。
// repr 形式要打成 b'...'；JSON 形式（/admin 的 detail 列表）序列化不了它 ——
// 线上是错误处理器自己抛异常，客户端看到 500，所以 validationErrorFor 要照抄这个崩法。
type pyBytes []byte

// decodeBody 对应 FastAPI 的 body 读取与校验，四种结局分开（都有金样本钉着）：
//
//	零字节 body            → 422 missing（`Body(default=…)` 则用空对象放行）
//	Content-Type 不是 JSON → body 当 bytes → 422 model_attributes_type（/admin 那边崩成 500）
//	JSON 读不通            → 422 json_invalid（loc 带字符偏移、ctx.error 是 CPython 的措辞）
//	合法但不是对象         → 422 model_attributes_type（input 就是那个值）
//
// 判定规则取自 fastapi/routing.py 的 get_request_handler：只有 media type 的 maintype 是
// application、subtype 等于 json 或以 +json 结尾时才会走 request.json()；
// 缺 Content-Type 时 strict_content_type 默认为 True，于是直接按 bytes 处理。
//
// 数字一律用 json.Number 承载：Python 的 1（int）与 1.0（float）在 str()、repr、
// 幂等哈希里写法不同，过成 Go float64 就分不出来了。
func decodeBody(r *http.Request) (map[string]any, error) { return decodeBodyWith(r, false) }

// decodeBodyWithDefault 对应 `payload: dict = Body(default={})`：没有 body 时用空对象放行。
func decodeBodyWithDefault(r *http.Request) (map[string]any, error) { return decodeBodyWith(r, true) }

func decodeBodyWith(r *http.Request, optional bool) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		if optional {
			return map[string]any{}, nil
		}
		return nil, validationErrorFor(r.URL.Path, []vErr{{Type: "missing",
			Loc: []any{"body"}, Msg: "Field required", Input: nil}})
	}
	if !contentTypeIsJSON(r.Header.Get("content-type")) {
		return nil, bodyRejectedBytes(r.URL.Path, raw)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, bodyJSONInvalid(r.URL.Path, string(raw))
	}
	if rest := strings.TrimSpace(string(raw)[dec.InputOffset():]); rest != "" {
		return nil, bodyJSONInvalid(r.URL.Path, string(raw))
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, validationErrorFor(r.URL.Path, []vErr{{Type: "model_attributes_type",
			Loc:   []any{"body"},
			Msg:   "Input should be a valid dictionary or object to extract fields from",
			Input: v}})
	}
	return m, nil
}

// contentTypeIsJSON 就是 FastAPI 那句 maintype=="application" && (subtype=="json" || subtype.endswith("+json"))。
func contentTypeIsJSON(ct string) bool {
	if ct == "" {
		return false
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	i := strings.IndexByte(media, '/')
	if i < 0 {
		return false
	}
	maintype, subtype := media[:i], media[i+1:]
	if maintype != "application" {
		return false
	}
	return subtype == "json" || strings.HasSuffix(subtype, "+json")
}

// bodyJSONInvalid 对应 FastAPI 捕获 json.JSONDecodeError 后自己拼的那条：
// loc 的第二段是字符偏移（e.pos），ctx.error 是 e.msg —— 都得由 pyJSONErr 复刻，
// 借 encoding/json 的报错会两头都不对。
func bodyJSONInvalid(path, body string) error {
	msg, pos := pyJSONErr(body)
	if msg == "" {
		// Go 的解码器拒了而扫描器觉得没问题：极少见的语法差异，位置按 0 报。
		msg, pos = "Expecting value", 0
	}
	return validationErrorFor(path, []vErr{{Type: "json_invalid",
		Loc: []any{"body", pos}, Msg: "JSON decode error", Input: map[string]any{},
		Ctx: map[string]any{"error": msg}}})
}

// bodyRejectedBytes：body 留在 bytes 形态，任何模型都收不下。
// /v1 的错误会被 repr 成字符串（'input': b'…'）所以还是 422；
// /admin 要把 input 原样序列化进 JSON，bytes 办不到 —— 线上错误处理器当场抛异常，客户端看到 500。
func bodyRejectedBytes(path string, raw []byte) error {
	if strings.HasPrefix(path, "/v1/") {
		return validationErrorFor(path, []vErr{{Type: "model_attributes_type",
			Loc: []any{"body"}, Msg: "Input should be a valid dictionary or object to extract fields from",
			Input: pyBytes(raw)}})
	}
	return errBytesUnserializable
}

var errBytesUnserializable = errors.New("bytes 型校验错误无法序列化成 JSON")

// manualJSONBody 是给 /v1/images/edits 的：它的签名是 `request: Request`，FastAPI 一层 body
// 校验都不做，handler 里自己 `await request.json()`。于是坏 JSON、空 body、
// 以及 `[]` / `"abc"` 这种「能解析但不是对象」（接下来那行 body.get 会抛 AttributeError）
// 全都是未捕获异常 → 500。edits_badjson 那条金样本钉住的就是这条路。
func manualJSONBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errBodyNotDict
	}
	return m, nil
}

var errBodyNotDict = errors.New("body 不是对象，.get 落空")

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

// errInputRepr：被拒的输入原样用 Python repr 打出来，但 None 缺字段时是 'None'，
// bytes（Content-Type 不是 JSON 那条路）是 b'...'。
func errInputRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case pyBytes:
		return bytesRepr(t)
	}
	return pyval.Repr(v)
}

// bytesRepr 是 Python 的 repr(b'...')：默认单引号，内容里有单引号而没有双引号时改用双引号；
// 反斜杠、引号、\t \n \r 用具名转义，0x20–0x7e 之外的（含非 ASCII）一律 \xNN 小写。
func bytesRepr(b []byte) string {
	q := byte('\'')
	if bytes.IndexByte(b, '\'') >= 0 && bytes.IndexByte(b, '"') < 0 {
		q = '"'
	}
	var sb strings.Builder
	sb.WriteString("b")
	sb.WriteByte(q)
	for _, c := range b {
		switch {
		case c == '\\':
			sb.WriteString(`\\`)
		case c == q:
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c == '\t':
			sb.WriteString(`\t`)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c >= 0x20 && c <= 0x7e:
			sb.WriteByte(c)
		default:
			const hex = "0123456789abcdef"
			sb.WriteString(`\x`)
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0xf])
		}
	}
	sb.WriteByte(q)
	return sb.String()
}

// validationErrorFor 是校验失败的对外形态：/v1/* 套 OpenAI 错误壳
// （message 是 str(exc.errors())[:400] 再截 400 字符），其它路由给 FastAPI 的 detail 列表。
// 写成自由函数是因为 body 读取那一路拿不到 App。
func validationErrorFor(path string, errs []vErr) error {
	if strings.HasPrefix(path, "/v1/") {
		return httpError(422, "请求参数校验失败："+validationRepr(errs))
	}
	list := make([]detailEntry, 0, len(errs))
	for _, e := range errs {
		list = append(list, e.detail())
	}
	return httpError(422, list)
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
