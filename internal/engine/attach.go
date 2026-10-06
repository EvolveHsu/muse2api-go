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
func (e *Engine) clickPoint(x, y int) {
	for _, t := range [2]string{"mousePressed", "mouseReleased"} {
		_, _ = e.page.Send("Input.dispatchMouseEvent", map[string]any{
			"type": t, "x": x, "y": y, "button": "left", "clickCount": 1}, 0)
	}
}

// Attachments 对应 attachments()：任何异常都退回空列表（与 Python 一致）。
func (e *Engine) Attachments() []Attachment {
	// Python 异常路径返回的是 []（不是 None），落盘/判空语义不同，必须保持一致。
	out := []Attachment{}
	if e.page == nil {
		return out
	}
	raw, err := e.page.JS(attJS, false, 0)
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
func (e *Engine) scrollBottom() {
	if e.page == nil {
		return
	}
	_, _ = e.page.JS(jsScrollBottomL649, false, 0)
}

// send 对应 _send()：先真点一下 textarea 拿焦点，再用 React 原型 setter 注入文字
// （超长上下文比 Input.insertText 逐字注入快得多），然后等 Send 按钮出现并点击；
// 失败再退到 Ctrl+Enter / Enter。返回 clicked | enter-sent | no-button。
func (e *Engine) send(prompt string) (string, error) {
	// 1. 让输入框进入视口并拿到真实焦点
	if _, err := e.page.JS(jsSendL559, false, 0); err != nil {
		// Python 里这一步被 except 吞掉，继续往下走
	}
	e.sleep(100 * time.Millisecond)

	rectVal, err := e.page.JS(jsSendL571, false, 0)
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
		if _, err := e.page.Send("Input.dispatchMouseEvent", map[string]any{
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
	if v, err := e.page.JS(setterExpr, false, 0); err == nil {
		valLen = v.Int()
	}
	// Python 用 len(prompt) 判断，那是字符数不是字节数：中文提示词按字节会早一个数量级跳出这条兜底。
	if valLen == 0 && utf8.RuneCountInString(prompt) < 500 {
		if _, err := e.page.Send("Input.insertText", map[string]any{"text": prompt}, 0); err != nil {
			return "", err
		}
		_, _ = e.page.JS(setterExpr, false, 0)
	}

	clicked := "no-button"
	deadline := e.now().Add(3 * time.Second)
	for e.now().Before(deadline) {
		res, err := e.page.JS(jsSendL605, false, 0)
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
				if _, err := e.page.Send("Input.dispatchKeyEvent", params, 0); err != nil {
					return "", err
				}
			}
			e.sleep(time.Second)
			v, err := e.page.JS(jsSendL631, false, 0)
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

// score 对应 _wait_attachment 里的 _score()：按期望类型给附件打分。
func scoreAttachment(a Attachment, expect string) int {
	tid := strings.ToLower(a.Tid)
	if expect == "video" {
		if a.HasVideo || strings.Contains(tid, "video") {
			return 2
		}
		return 1
	}
	if strings.Contains(tid, "image") || !a.HasVideo {
		return 2
	}
	return 1
}

// WaitAttachment 对应 _wait_attachment()：轮询新附件，附带三条快速失败/宽限逻辑。
// 返回 (附件, 是否拿到, 错误)；错误对应 Python 的 raise。
func (e *Engine) WaitAttachment(baselineSrc string, timeout int, expect string,
	onProgress func(int), baseAgentCnt, baseAttCnt int, stop <-chan struct{},
	baselineSources map[string]bool) (Attachment, bool, error) {

	if baselineSources == nil {
		baselineSources = map[string]bool{}
	}
	baselineSources[baselineSrc] = true
	deadline := e.now().Add(time.Duration(timeout) * time.Second)
	tStart := e.now()
	stableSrc := ""
	stableN := 0
	lastTxt := ""
	txtStable := 0
	fallbackSince := 0.0

	for e.now().Before(deadline) {
		if stop != nil {
			select {
			case <-stop:
				return Attachment{}, false, &ErrGeneration{Msg: "客户端已断开连接，终止生成任务"}
			default:
			}
		}
		e.sleep(600 * time.Millisecond)
		e.scrollBottom()
		atts := e.Attachments()

		// 视频请求优先挑带 video 的附件；muse 有时先渲染封面静帧，所以先记下图当作兜底，
		// 再给视频节点一段宽限期，避免把封面 img 误当成品返回。
		var att Attachment
		var fallbackAtt Attachment
		haveAtt, haveFallback := false, false
		if len(atts) > 0 {
			best := atts[0]
			bestScore := scoreAttachment(best, expect)
			for _, a := range atts[1:] {
				if s := scoreAttachment(a, expect); s > bestScore {
					best, bestScore = a, s
				}
			}
			if bestScore >= 2 {
				att, haveAtt = best, true
			} else {
				fallbackAtt, haveFallback = atts[len(atts)-1], true
			}
		}
		if !haveAtt && haveFallback && expect == "video" {
			if fallbackSince == 0.0 {
				fallbackSince = float64(e.now().UnixNano()) / 1e9
			}
			if float64(e.now().UnixNano())/1e9-fallbackSince >= 15.0 {
				att, haveAtt = fallbackAtt, true
			}
		}
		if haveAtt {
			src := att.Src
			vSrc := att.VSrc
			tid := att.Tid
			isFallback := haveFallback && att == fallbackAtt &&
				!(att.HasVideo || strings.Contains(strings.ToLower(tid), "video")) && expect == "video"
			var want bool
			switch {
			case isFallback:
				want = true // 宽限期已过，接受图片兜底结果
			case expect == "video":
				want = att.HasVideo || strings.Contains(tid, "video") ||
					strings.Contains(src, "video") || strings.Contains(vSrc, "video") ||
					strings.HasSuffix(src, ".mp4") || strings.HasSuffix(src, ".webm") ||
					strings.HasSuffix(src, ".mov")
			default:
				want = strings.Contains(tid, "image") || !att.HasVideo
			}
			checkSrc := src
			if expect == "video" && vSrc != "" {
				checkSrc = vSrc
			}
			if checkSrc != "" && !baselineSources[checkSrc] && want {
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

		var st chatState
		raw, err := e.page.JS(jsWaitAttachmentL735, false, 0)
		if err == nil && raw.Truthy() {
			_ = json.Unmarshal([]byte(raw.Str2()), &st)
		}
		if creditsRe.MatchString(st.Tail) {
			return Attachment{}, false, &ErrGeneration{Msg: "账号额度不足"}
		}
		// 侧边栏/失效连接的文案不足以判定本次生成失败，仍然由调用方的 timeout 兜底。
		// 快速失败：助手已完成纯文字回复（无 Stop 按钮且无新附件），且不是正在产出媒体的报告。
		if st.Cnt > baseAgentCnt && st.Txt != "" && !st.Stop && len(atts) <= baseAttCnt {
			if !mediaRepoRe.MatchString(st.Txt) {
				if st.Txt == lastTxt {
					txtStable++
				} else {
					lastTxt, txtStable = st.Txt, 0
				}
				if txtStable >= 15 && elapsed > 8.0 {
					clip := []rune(st.Txt)
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
func (e *Engine) debugDump(tag string) {
	if e.page == nil {
		return
	}
	info, err := e.page.JS(assembleDebugDumpL1202(), false, 0)
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
		case r < 0x80:
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
