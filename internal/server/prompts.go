package server

import (
	"encoding/json"
	"sort"
	"strings"
)

// 这一堆请求模型对应 app.py 里的 pydantic 类。
// 字段顺序 = Python 声明顺序（model_dump 的键序、以及 _queue_image 算 request_hash 时的
// 参与集合都要按这个顺序来）；默认值也逐条照抄。
//
// 数字一律用 json.Number 承载（见 decodeBody），这样 Python 的 1（int）与 1.0（float）
// 在回填响应、算幂等哈希、str() 转换时才不会串味。

type ImageRequest struct {
	Prompt         string       `json:"prompt"`
	Model          string       `json:"model"`
	N              json.Number  `json:"n"`
	Size           *string      `json:"size"`
	AspectRatio    *string      `json:"aspect_ratio"`
	ResponseFormat string       `json:"response_format"`
	Timeout        *json.Number `json:"timeout"`
	Extra          *string      `json:"extra"`
	Image          any          `json:"image"`
	Images         []any        `json:"images"`
	Reference      *string      `json:"reference_image"`
	Async          bool         `json:"async"`
}

func newImageRequest() *ImageRequest {
	return &ImageRequest{Model: "muse-image", N: json.Number("1"),
		ResponseFormat: "url"}
}

type VideoRequest struct {
	Prompt      string       `json:"prompt"`
	Model       string       `json:"model"`
	Duration    *json.Number `json:"duration"`
	Size        *string      `json:"size"`
	AspectRatio *string      `json:"aspect_ratio"`
	Resolution  *string      `json:"resolution"`
	Timeout     *json.Number `json:"timeout"`
	Extra       *string      `json:"extra"`
	Image       any          `json:"image"`
	ImageURL    any          `json:"image_url"`
	Reference   *string      `json:"reference_image"`
}

func newVideoRequest() *VideoRequest { return &VideoRequest{Model: "muse-video"} }

type ChatMessage struct {
	Role      string  `json:"role"`
	Content   any     `json:"content"`
	Name      *string `json:"name"`
	ToolCall  *string `json:"tool_call_id"`
	ToolCalls any     `json:"tool_calls"`
}

type ChatRequest struct {
	Model          string        `json:"model"`
	Messages       []ChatMessage `json:"messages"`
	Stream         bool          `json:"stream"`
	Temperature    *json.Number  `json:"temperature"`
	MaxTokens      *json.Number  `json:"max_tokens"`
	Timeout        *json.Number  `json:"timeout"`
	Prompt         *string       `json:"prompt"`
	Tools          any           `json:"tools"`
	ToolChoice     any           `json:"tool_choice"`
	ResponseFormat any           `json:"response_format"`
	StreamOptions  any           `json:"stream_options"`
}

func newChatRequest() *ChatRequest { return &ChatRequest{Model: "muse-spark"} }

type ResponsesRequest struct {
	Model        string       `json:"model"`
	Input        any          `json:"input"`
	Instructions *string      `json:"instructions"`
	Stream       bool         `json:"stream"`
	MaxOutput    *json.Number `json:"max_output_tokens"`
	Timeout      *json.Number `json:"timeout"`
	Tools        any          `json:"tools"`
	Store        *bool        `json:"store"`
}

func newResponsesRequest() *ResponsesRequest { return &ResponsesRequest{Model: "muse-spark"} }

// MODEL_ALIASES 逐条抄自 app.py；条数由 TestModelAliasesCount 对账金样本钉住。
var modelAliases = map[string]string{}

func init() {
	for k, v := range map[string]string{
		"muse-text": "muse-spark", "muse-chat": "muse-spark", "muse-llm": "muse-spark",
		"koda":          "muse-spark",
		"gpt-3.5-turbo": "muse-spark", "gpt-4": "muse-spark", "gpt-4-turbo": "muse-spark",
		"gpt-4o": "muse-spark", "gpt-4o-mini": "muse-spark", "gpt-4.1": "muse-spark",
		"gpt-4.1-mini": "muse-spark", "gpt-5": "muse-spark", "gpt-5-codex": "muse-spark",
		"o1": "muse-spark", "o1-mini": "muse-spark", "o3": "muse-spark",
		"o3-mini": "muse-spark", "o4-mini": "muse-spark",
		"codex": "muse-spark", "codex-mini-latest": "muse-spark",
		"claude-3-5-sonnet": "muse-spark", "claude-3-5-sonnet-latest": "muse-spark",
		"claude-3-7-sonnet": "muse-spark", "claude-sonnet-4": "muse-spark",
		"claude-opus-4": "muse-spark", "claude-3-opus": "muse-spark",
		"claude-3-haiku": "muse-spark",
		"deepseek-chat":  "muse-spark", "deepseek-coder": "muse-spark",
		"deepseek-reasoner": "muse-spark", "qwen-coder": "muse-spark",
		"gemini-2.5-pro": "muse-spark", "gemini-2.5-flash": "muse-spark",
		"muse-img": "muse-image", "dall-e": "muse-image", "dall-e-3": "muse-image",
		"gpt-image-1": "muse-image", "flux": "muse-image", "midjourney": "muse-image",
		"muse-vid": "muse-video", "muse-videos": "muse-video",
		"sora": "muse-video", "sora-2": "muse-video", "veo": "muse-video",
		"veo-3": "muse-video", "kling": "muse-video", "runway": "muse-video",
	} {
		modelAliases[k] = v
	}
}

// resolveModel 对应 resolve_model()：大小写与首尾空格都先抹平，认不出来就原样返回。
func resolveModel(name *string, def string) string {
	n := strings.ToLower(strings.TrimSpace(deref(name)))
	if v, ok := modelAliases[n]; ok {
		return v
	}
	if n == "" {
		return def
	}
	return n
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func containsAny(hay string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

var verticalKeys = []string{"9:16", "9/16", "portrait", "竖屏", "720x1280", "1080x1920"}
var landscapeKeys = []string{"16:9", "16/9", "landscape", "横屏", "1280x720", "1920x1080"}
var squareKeys = []string{"1:1", "square", "正方形", "1024x1024"}

// buildImagePrompt 对应 build_image_prompt()。分支顺序就是判定的先后，不能重排。
func buildImagePrompt(r *ImageRequest) string {
	hasRef := truthy(r.Reference) || truthy(r.Image) || truthy(r.Images)
	var parts []string
	if hasRef {
		parts = append(parts, "基于我本次上传附带的参考图片进行生图/编辑："+strings.TrimSpace(r.Prompt))
	} else {
		parts = append(parts, "全新文生图创作（当前未提供任何参考图，请勿查找历史相册或向用户索要原图，"+
			"直接根据文字描述从零绘制生成一张全新图片）："+strings.TrimSpace(r.Prompt))
	}
	ar := strings.ToLower(strings.TrimSpace(deref(r.AspectRatio)))
	sz := strings.ToLower(strings.TrimSpace(deref(r.Size)))
	switch {
	case containsAny(ar, verticalKeys) || containsAny(sz, verticalKeys):
		parts = append(parts, "【画面构图与比例要求】：严格 9:16 竖屏满屏画幅（9:16 vertical portrait aspect ratio，"+
			"高大于宽的手机全屏竖版画面），绝对不要生成横屏，保持垂直构图")
	case containsAny(ar, landscapeKeys) || containsAny(sz, landscapeKeys):
		parts = append(parts, "【画面构图与比例要求】：16:9 宽屏横屏画幅（16:9 widescreen landscape aspect ratio）")
	case containsAny(ar, squareKeys) || containsAny(sz, squareKeys):
		parts = append(parts, "【画面构图与比例要求】：1:1 正方形画幅（1:1 square aspect ratio）")
	case containsAny(ar, []string{"4:3", "4/3"}) || containsAny(sz, []string{"4:3", "4/3"}):
		parts = append(parts, "【画面构图与比例要求】：4:3 比例画幅")
	case containsAny(ar, []string{"3:4", "3/4"}) || containsAny(sz, []string{"3:4", "3/4"}):
		parts = append(parts, "【画面构图与比例要求】：3:4 竖向画幅")
	case truthy(r.AspectRatio):
		parts = append(parts, "【画面构图与比例要求】："+*r.AspectRatio+" 画面比例")
	case truthy(r.Size):
		parts = append(parts, "尺寸/比例："+*r.Size)
	}
	if hasRef {
		parts = append(parts, "【纯净画面要求】：彻底清除并去除参考图中的所有文字、水印、签名、角标及Logo标记"+
			"（clean image without any watermark, text, or logo），输出绝对纯净无字画面")
	}
	if truthy(r.Extra) {
		parts = append(parts, *r.Extra)
	}
	return strings.Join(parts, "，")
}

// buildVideoPrompt 对应 build_video_prompt()。
func buildVideoPrompt(r *VideoRequest) string {
	user := strings.TrimSpace(r.Prompt)
	ar := strings.ToLower(strings.TrimSpace(deref(r.AspectRatio)))
	sz := strings.ToLower(strings.TrimSpace(deref(r.Size)))
	dur := "6"
	if truthy(r.Duration) {
		dur = r.Duration.String()
	}
	hasRef := truthy(r.Reference) || truthy(r.Image) || truthy(r.ImageURL)
	vertical := containsAny(ar, verticalKeys) || containsAny(sz, verticalKeys)

	var parts []string
	switch {
	case vertical:
		if hasRef {
			parts = append(parts, "基于我本次上传的参考图片附件作为第一帧参考图（严禁使用历史图片或任何其他图像，"+
				"必须严格以我当前刚刚上传并附带在此处的这张图片为起始帧）：生成一个严格9:16竖屏手机满屏的动态图生视频"+
				"（9:16 vertical portrait video，720x1280，高大于宽的手机全屏竖版画面，严格以附带的参考图为起始第一帧延续动作，"+
				"严禁生成横屏或黑边，时长严格为 "+dur+" 秒）："+user)
		} else {
			parts = append(parts, "全新文生视频创作（纯文本全新生成，严禁参考任何历史图片或上下文）：生成一个严格9:16竖屏手机满屏视频"+
				"（9:16 vertical portrait video，720x1280，高大于宽的手机全屏竖版画面，严禁生成横屏或带有左右黑边，保持垂直构图，"+
				"时长严格为 "+dur+" 秒）："+user)
		}
	case containsAny(ar, landscapeKeys) || containsAny(sz, landscapeKeys):
		if hasRef {
			parts = append(parts, "基于我本次上传的参考图片附件作为第一帧参考图（严禁使用历史图片或任何其他图像，"+
				"必须严格以我当前刚刚上传并附带在此处的这张图片为起始帧）：生成一个16:9宽屏横屏图生视频"+
				"（16:9 widescreen landscape video，严格以附带的参考图为起始第一帧延续动作，时长严格为 "+dur+" 秒）："+user)
		} else {
			parts = append(parts, "全新文生视频创作（纯文本全新生成，严禁参考任何历史图片或上下文）：生成一个16:9横屏宽屏视频"+
				"（16:9 widescreen landscape video，时长严格为 "+dur+" 秒）："+user)
		}
	default:
		if hasRef {
			parts = append(parts, "基于我本次上传的参考图片附件作为第一帧参考图（严禁使用历史图片或任何其他图像，"+
				"必须严格以我当前刚刚上传并附带在此处的这张图片为起始帧）：生成动态图生视频"+
				"（严格以附带的参考图为起始第一帧延续动作，时长严格为 "+dur+" 秒）："+user)
		} else {
			parts = append(parts, "全新文生视频创作（纯文本全新生成，严禁参考任何历史图片或上下文）：生成一个视频"+
				"（时长严格为 "+dur+" 秒）："+user)
		}
	}
	if truthy(r.Resolution) {
		parts = append(parts, "画质规格："+*r.Resolution)
	}
	if truthy(r.Extra) {
		parts = append(parts, *r.Extra)
	}
	return strings.Join(parts, "，")
}

// truthy 就是 Python 的 truthiness：nil/空串/0/空集合/False 为假。
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case *string:
		return t != nil && *t != ""
	case json.Number:
		f, err := t.Float64()
		return err == nil && f != 0
	case *json.Number:
		return t != nil && truthy(*t)
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case []ChatMessage:
		return len(t) > 0
	}
	return v != nil
}

// contentText 对应 _content_text()。
func contentText(content any) string {
	if content == nil {
		return ""
	}
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			switch x := item.(type) {
			case map[string]any:
				// Python 是 t = item.get("type")，然后 t in (None, "text", "input_text", "output_text")：
				// 键缺失与显式 null 都算 None。
				t := x["type"]
				if t == nil || t == "text" || t == "input_text" || t == "output_text" {
					parts = append(parts, pyStr(orEmpty(x["text"])))
				} else if t == "image_url" {
					parts = append(parts, "[图片]")
				}
			case string:
				parts = append(parts, x)
			}
		}
		var kept []string
		for _, p := range parts {
			if p != "" {
				kept = append(kept, p)
			}
		}
		return strings.Join(kept, "\n")
	}
	return pyStr(content)
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// buildChatPrompt 对应 build_chat_prompt()。
func buildChatPrompt(messages []ChatMessage) string {
	var system []string
	type turn struct{ role, text string }
	var turns []turn
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := strings.TrimSpace(contentText(m.Content))
		if role == "tool" {
			if text != "" {
				turns = append(turns, turn{"user", "【工具执行结果】\n" + text})
			}
			continue
		}
		if text == "" {
			if role == "assistant" && truthy(m.ToolCalls) {
				turns = append(turns, turn{"assistant", "【请求调用工具】" +
					truncate(dumpsJSON(m.ToolCalls, false), 600)})
			}
			continue
		}
		if role == "system" || role == "developer" {
			system = append(system, text)
		} else {
			turns = append(turns, turn{role, text})
		}
	}
	if len(turns) == 1 && len(system) == 0 && turns[0].role == "user" {
		return turns[0].text
	}
	var parts []string
	if len(system) > 0 {
		sys := strings.Join(system, "\n\n")
		sys = strings.ReplaceAll(sys, "danger-full-access", "standard-workspace-access")
		parts = append(parts, "背景与任务设定：\n"+sys)
	}
	for _, t := range turns {
		label := "用户"
		if t.role == "assistant" {
			label = "助手"
		}
		parts = append(parts, label+"：\n"+t.text)
	}
	return strings.Join(parts, "\n\n")
}

// responsesMessages 对应 _responses_messages()。
func responsesMessages(req *ResponsesRequest) []ChatMessage {
	var msgs []ChatMessage
	if truthy(req.Instructions) {
		msgs = append(msgs, ChatMessage{Role: "system", Content: deref(req.Instructions)})
	}
	switch inp := req.Input.(type) {
	case string:
		return append(msgs, ChatMessage{Role: "user", Content: inp})
	case []any:
		for _, item := range inp {
			switch x := item.(type) {
			case string:
				msgs = append(msgs, ChatMessage{Role: "user", Content: x})
				continue
			case map[string]any:
				itype, _ := x["type"].(string)
				if role, ok := x["role"].(string); ok && role != "" {
					msgs = append(msgs, ChatMessage{Role: role, Content: x["content"]})
				} else if itype == "input_text" {
					msgs = append(msgs, ChatMessage{Role: "user", Content: x["text"]})
				} else if itype == "function_call_output" {
					msgs = append(msgs, ChatMessage{Role: "user",
						Content: "【工具执行结果】\n" + pyStr(orEmpty(x["output"]))})
				} else if itype == "function_call" {
					msgs = append(msgs, ChatMessage{Role: "assistant",
						Content: "【请求调用工具】" + pyStr(orEmpty(x["name"]))})
				}
			}
		}
	}
	return msgs
}

var _ = sort.Strings
