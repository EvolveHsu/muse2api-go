package server

import (
	"strings"
	"time"

	"muse2api-go/internal/pyval"
)

// pyStr / truncate 是 Python 的 str() 与 [:n]（按字符数，不是字节数）。
func pyStr(v any) string { return pyval.Str(v) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func dumpsJSON(v any, ensureASCII bool) string {
	return pyval.Dumps(v, ensureASCII, false)
}

// mediaURL 对应 media_url()：配了 public_base 就出绝对 URL ——
// OpenAI 兼容客户端拿到 data[].url 会直接渲染，相对路径会被解析到客户端自己的域名上。
// 注意文件名不做 URL 编码，与 Python 的 f-string 一致（金样本 media_url_public_slash 钉着）。
func (a *App) mediaURL(name string) string {
	base := a.publicBase()
	if base != "" {
		return base + "/v1/media/" + name
	}
	return "/v1/media/" + name
}

func (a *App) publicBase() string {
	return strings.TrimRight(a.CFG.PublicBase, "/")
}

// sse 对应 _sse()：分隔符是 Python json.dumps 的默认值（", " 与 ": "），不是紧凑写法。
func sse(v any) string {
	return "data: " + pyval.Dumps(v, false, false) + "\n\n"
}

var sseHeaders = [][2]string{
	{"Cache-Control", "no-cache, no-transform"},
	{"X-Accel-Buffering", "no"},
	{"Connection", "keep-alive"},
}

func chatChunk(id string, created int64, model string, delta pyval.Obj, finish any) pyval.Obj {
	return pyval.Obj{
		{"id", id}, {"object", "chat.completion.chunk"}, {"created", created},
		{"model", model},
		{"choices", []any{pyval.Obj{
			{"index", 0}, {"delta", delta}, {"finish_reason", finish},
		}}},
	}
}

// wantUsage 对应 _want_usage()：下游带了 stream_options.include_usage 就要求在 [DONE]
// 之前补一个 choices: [] + usage 的分片，否则某些框架会一直等 usage 卡住。
func wantUsage(opts any) bool {
	if m, ok := opts.(map[string]any); ok {
		return truthy(m["include_usage"])
	}
	return false
}

// paceText 对应 _pace_text()：分片长度按**字符**算，不是字节。
// 少于等于 3 个字符直接放行；delay<=0 时也一样整块给出。
func paceEmit(text string, chunkSize int, delay time.Duration, emit func(string) error) error {
	if text == "" {
		return nil
	}
	r := []rune(text)
	if len(r) <= 3 || delay <= 0 {
		return emit(text)
	}
	for i := 0; i < len(r); i += chunkSize {
		end := i + chunkSize
		if end > len(r) {
			end = len(r)
		}
		if err := emit(string(r[i:end])); err != nil {
			return err
		}
		if end < len(r) {
			time.Sleep(delay)
		}
	}
	return nil
}
