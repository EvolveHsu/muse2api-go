package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Attachment 是 _ATT_JS 回传的生成结果描述，字段名与 JS 里的 camelCase 保持一致。
type Attachment struct {
	Tid      string `json:"tid"`
	HasVideo bool   `json:"hasVideo"`
	HasImg   bool   `json:"hasImg"`
	Src      string `json:"src"`
	VSrc     string `json:"vSrc"`
	ISrc     string `json:"iSrc"`
	W        int    `json:"w"`
	H        int    `json:"h"`
}

// clickPoint 对应 _click_point()。
func (e *Engine) clickPoint(page Session, x, y int) {
	for _, t := range [2]string{"mousePressed", "mouseReleased"} {
		_, _ = page.Send("Input.dispatchMouseEvent", map[string]any{
			"type": t, "x": x, "y": y, "button": "left", "clickCount": 1}, 0)
	}
}

// Attachments 对应 attachments()：任何异常都退回空列表（与 Python 一致）。
func (e *Engine) Attachments(page Session) []Attachment {
	// Python 异常路径返回的是 []（不是 None），落盘/判空语义不同，必须保持一致。
	out := []Attachment{}
	if page == nil {
		return out
	}
	raw, err := page.JS(attJS, false, 0)
	if err != nil || !raw.Truthy() {
		return out
	}
	if err := json.Unmarshal([]byte(raw.Str2()), &out); err != nil {
		return []Attachment{}
	}
	return out
}

// scrollBottom 对应 _scroll_bottom()：muse.ai 的滚动容器是内层 div，
// 虚拟列表按滚动位置渲染，不滚到底新消息根本不在 DOM 里。
func (e *Engine) scrollBottom(page Session) {
	if page == nil {
		return
	}
	_, _ = page.JS(jsScrollBottom, false, 0)
}

// send 对应 _send()：先真点一下 textarea 拿焦点，再用 React 原型 setter 注入文字
// （超长上下文比 Input.insertText 逐字注入快得多），然后等 Send 按钮出现并点击；
// 失败再退到 Ctrl+Enter / Enter。返回 clicked | enter-sent | no-button。
func (e *Engine) send(page Session, prompt string) (string, error) {
	// 1. 让输入框进入视口并拿到真实焦点
	if _, err := page.JS(jsFocusInput, false, 0); err != nil {
		// Python 里这一步被 except 吞掉，继续往下走
	}
	e.sleep(100 * time.Millisecond)

	rectVal, err := page.JS(jsInputRect, false, 0)
	if err != nil {
		return "", err
	}
	if !rectVal.Truthy() {
		return "", &ErrGeneration{Msg: "找不到聊天输入框"}
	}
	var pt struct{ X, Y int }
	if err := json.Unmarshal([]byte(rectVal.Str2()), &pt); err != nil {
		return "", &ErrGeneration{Msg: "找不到聊天输入框"}
	}
	for _, t := range [2]string{"mousePressed", "mouseReleased"} {
		if _, err := page.Send("Input.dispatchMouseEvent", map[string]any{
			"type": t, "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1}, 0); err != nil {
			return "", err
		}
	}
	e.sleep(100 * time.Millisecond)

	setterExpr, err := pyFormat(setterJS, jsonString(prompt))
	if err != nil {
		return "", &ErrGeneration{Msg: err.Error()}
	}
	valLen := 0
	if v, err := page.JS(setterExpr, false, 0); err == nil {
		valLen = v.Int()
	}
	// Python 用 len(prompt) 判断，那是字符数不是字节数：中文提示词按字节会早一个数量级跳出这条兜底。
	if valLen == 0 && utf8.RuneCountInString(prompt) < 500 {
		if _, err := page.Send("Input.insertText", map[string]any{"text": prompt}, 0); err != nil {
			return "", err
		}
		_, _ = page.JS(setterExpr, false, 0)
	}

	clicked := "no-button"
	deadline := e.now().Add(3 * time.Second)
	for e.now().Before(deadline) {
		res, err := page.JS(jsSendButton, false, 0)
		if err != nil {
			return "", err
		}
		if res.Str2() == "clicked" {
			clicked = "clicked"
			break
		}
		e.sleep(80 * time.Millisecond)
	}

	if clicked != "clicked" {
		for _, mods := range [2]int{1, 0} {
			for _, t := range [3]string{"keyDown", "char", "keyUp"} {
				params := map[string]any{
					"type": t, "key": "Enter", "code": "Enter",
					"windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13,
					"modifiers": mods,
				}
				if t == "char" {
					params["text"] = "\r"
					params["unmodifiedText"] = "\r"
				}
				if _, err := page.Send("Input.dispatchKeyEvent", params, 0); err != nil {
					return "", err
				}
			}
			e.sleep(time.Second)
			v, err := page.JS(jsInputCleared, false, 0)
			if err == nil && v.Truthy() {
				clicked = "enter-sent"
				break
			}
		}
	}
	e.sleep(300 * time.Millisecond)
	return clicked, nil
}

// chatState 是 _wait_attachment 里那段状态探测 JS 的返回值。
type chatState struct {
	Cnt  int    `json:"cnt"`
	Txt  string `json:"txt"`
	Stop bool   `json:"stop"`
	Tail string `json:"tail"`
}

var (
	creditsRe   = regexp.MustCompile(`额度不足|积分不足|out of credits|达到上限|token limit`)
	mediaRepoRe = regexp.MustCompile(`(?i)\.(?:webp|png|jpe?g|mp4|webm)|imagine_media|deliverable|generated\s+.*image|verified\s+generated|artifact`)
)

// videoExtRe 是「这看起来像一条真实视频源」的扩展名判定。
var videoExtRe = regexp.MustCompile(`\.(mp4|webm|mov)$`)

// stillSendingRe 对应上游 1.6.0 新增的「提示词根本没送达网关」快速失败关键词。
var stillSendingRe = regexp.MustCompile(`Still sending|Delivery not confirmed|正在发送|未确认送达`)

// WaitAttachment 对应上游 1.6.0 重写后的 _wait_attachment()：
//   - 严格按期望类型挑附件，绝不接受跨模态兜底（视频请求不再用封面静帧冒充成品）；
//   - 新增「30 秒仍未送达后端（Still sending / Delivery not confirmed）」的快速失败。
//
// 返回 (附件, 是否拿到, 错误)；错误对应 Python 的 raise。
func (e *Engine) WaitAttachment(page Session, baselineSrc string, timeout float64, expect string,
	onProgress func(int), baseAgentCnt, baseAttCnt int, stop <-chan struct{},
	baselineSources map[string]bool) (Attachment, bool, error) {

	if baselineSources == nil {
		baselineSources = map[string]bool{}
	}
	baselineSources[baselineSrc] = true
	deadline := e.now().Add(time.Duration(timeout * float64(time.Second)))
	tStart := e.now()
	stableSrc := ""
	stableN := 0
	lastTxt := ""
	txtStable := 0

	for e.now().Before(deadline) {
		if stop != nil {
			select {
			case <-stop:
				return Attachment{}, false, &ErrGeneration{Msg: "客户端已断开连接，终止生成任务"}
			default:
			}
		}
		e.sleep(600 * time.Millisecond)
		e.scrollBottom(page)
		atts := e.Attachments(page)

		// 严格按期望类型挑候选：视频只认真实视频源（vSrc / hasVideo / 视频扩展名）。
		// tid 含 "video" 不足以命中 —— muse 会先把 data-testid 标成 ...-presentation-video，
		// 此时 <video> 可能尚未挂上，_ATT_JS 的 primary = v || img 会退回封面静帧。
		var matching []Attachment
		for _, a := range atts {
			tid := strings.ToLower(a.Tid)
			src := a.Src
			vSrc := a.VSrc
			match := false
			if expect == "video" {
				match = a.HasVideo || vSrc != "" || videoExtRe.MatchString(src)
			} else {
				match = strings.Contains(tid, "image") ||
					(!a.HasVideo && !videoExtRe.MatchString(src))
			}
			if match {
				matching = append(matching, a)
			}
		}
		var att Attachment
		haveAtt := false
		if len(matching) > 0 {
			att, haveAtt = matching[len(matching)-1], true
		}
		if haveAtt {
			src := att.Src
			vSrc := att.VSrc
			// 视频任务禁止用封面 src 冒充成品；无 vSrc 时只接受显式视频后缀的 src。
			//
			// 变异验证留下的事实，记下来免得被误读成覆盖缺口：上面那段 is_match 与这里的
			// checkSrc 是**两道各自都能独立挡住静帧的闸门**——把 "video" in tid 加回去、
			// 或把 1.5.x 的 15 秒宽限兜底加回来，回放都不会红（另一道照样拦得住），
			// 要复现旧行为必须同时放宽两道。真正各自唯一能咬住的是：wait_att_video_src_ext_only
			// 钉住 videoExtRe 这一路（去掉它就红），poster_rejected 钉住拒绝交付。
			checkSrc := src
			if expect == "video" {
				switch {
				case vSrc != "":
					checkSrc = vSrc
				case videoExtRe.MatchString(src):
					checkSrc = src
				default:
					checkSrc = ""
				}
			}
			if checkSrc != "" && !baselineSources[checkSrc] {
				if att.W > 0 && att.H > 0 {
					return att, true, nil
				}
				if checkSrc == stableSrc {
					stableN++
				} else {
					stableSrc, stableN = checkSrc, 0
				}
				if stableN >= 1 {
					return att, true, nil
				}
			}
		}

		elapsed := e.now().Sub(tStart).Seconds()
		if onProgress != nil {
			prog := int(25 + elapsed*1.0)
			if prog > 92 {
				prog = 92
			}
			onProgress(prog)
		}

		// 探测变量先给默认值：JS 探测失败时后面的判定按「没有新消息」处理（上游 1.6.0 同义）。
		curCnt := baseAgentCnt
		curTxt := ""
		hasStop := false
		tail := ""
		var st chatState
		raw, err := page.JS(jsWaitAttachmentState, false, 0)
		if err == nil && raw.Truthy() {
			if json.Unmarshal([]byte(raw.Str2()), &st) != nil {
				st = chatState{}
			}
		}
		tail = st.Tail
		if creditsRe.MatchString(tail) {
			return Attachment{}, false, &ErrGeneration{Msg: "账号额度不足"}
		}
		// A Stop button is also shown for messages never acknowledged by the gateway.
		// 注意：上游这一条比较的 cur_cnt 还没被 st 覆盖（= base_agent_cnt），
		// 因此事实条件就是 elapsed>30 且 tail 命中关键词；这里照抄，不自作主张"修正"。
		if elapsed > 30.0 && curCnt == baseAgentCnt && stillSendingRe.MatchString(tail) {
			return Attachment{}, false, &ErrGeneration{Msg: "提示词未成功送达后端（Still sending / Delivery not confirmed），终止等待并重试"}
		}
		curCnt = st.Cnt
		curTxt = st.Txt
		hasStop = st.Stop
		// 快速失败：助手已完成纯文字回复（无 Stop 按钮且无新附件），且不是正在产出媒体的报告。
		if curCnt > baseAgentCnt && curTxt != "" && !hasStop && len(atts) <= baseAttCnt {
			if !mediaRepoRe.MatchString(curTxt) {
				if curTxt == lastTxt {
					txtStable++
				} else {
					lastTxt, txtStable = curTxt, 0
				}
				if txtStable >= 15 && elapsed > 8.0 {
					clip := []rune(curTxt)
					if len(clip) > 120 {
						clip = clip[:120]
					}
					return Attachment{}, false, &ErrGeneration{Msg: "模型未生成媒体，仅返回文本: " + string(clip)}
				}
			} else {
				txtStable = 0
			}
		} else {
			txtStable = 0
		}
	}
	return Attachment{}, false, nil
}

var debugFileRe = regexp.MustCompile(`debug-(.*)\.json$`)

// debugDump 对应 _debug_dump()：失败时把 DOM 现状写到 data/debug-<tag>.json，任何异常都吞掉。
func (e *Engine) debugDump(page Session, tag string) {
	if page == nil {
		return
	}
	info, err := page.JS(assembleDebugDump(), false, 0)
	if err != nil {
		return
	}
	text := pythonRepr(info)
	_ = os.WriteFile(filepath.Join(e.CFG.DataDir(), "debug-"+tag+".json"), []byte(text), 0o644)
}

// pythonRepr 对应 Python 把 CDP 返回值直接 str() 进文件的结果。
func pythonRepr(v Value) string {
	switch v.Kind {
	case KString:
		return v.Str
	case KNull:
		return "None"
	case KBool:
		if v.Bool {
			return "True"
		}
		return "False"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v.Str2()
	}
	var back any
	if err := json.Unmarshal(raw, &back); err != nil {
		return v.Str2()
	}
	return pyRepr(back)
}

func pyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return "'" + strings.ReplaceAll(strings.ReplaceAll(t, `\`, `\\`), "'", `\'`) + "'"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, pyRepr(x))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortStrings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, "'"+k+"': "+pyRepr(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// jsonString 复刻 Python 的 json.dumps(str)：只转义控制字符、引号与反斜杠，
// 非 ASCII 一律转成 \uXXXX（ensure_ascii=True），且 **不** 转义 < > &
// —— Go 的 json.Marshal 默认会把它们转成 \u003c，那样注入表达式就跟 Python 不一致了。
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r <= 0x7E:
			// CPython 的判据是 ESCAPE_ASCII = ([\\"]|[^\ -~])，
			// 所以「空格~波浪号」原样，0x7F（DEL）要转义。
			b.WriteRune(r)
		case r > 0xFFFF:
			r1 := 0xD800 + ((r - 0x10000) >> 10)
			r2 := 0xDC00 + ((r - 0x10000) & 0x3FF)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
