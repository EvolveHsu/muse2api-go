package engine

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// agentText 对应 _agent_text()：最后一个助手气泡的文本。
func (e *Engine) agentText() string {
	v, err := e.page.JS(agentTextJS, false, 0)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v.Str2())
}

// agentCount 对应 _agent_count()：JS 返回的是字符串数字，取不到算 0。
func (e *Engine) agentCount() int {
	v, err := e.page.JS(agentCountJS, false, 0)
	if err != nil || !v.Truthy() {
		return 0
	}
	n, e2 := strconv.Atoi(strings.TrimSpace(v.Str2()))
	if e2 != nil {
		return 0
	}
	return n
}

// userCount 对应 _user_count()。
func (e *Engine) userCount() int {
	v, err := e.page.JS(userCountJS, false, 0)
	if err != nil || !v.Truthy() {
		return 0
	}
	n, e2 := strconv.Atoi(strings.TrimSpace(v.Str2()))
	if e2 != nil {
		return 0
	}
	return n
}

// pollChat 对应 _poll_chat()：一次 CDP 同时完成"滚到底 + 读气泡"，异常退化成零值。
func (e *Engine) pollChat() (int, string, bool) {
	raw, err := e.page.JS(pollChatJS, false, 0)
	if err != nil || !raw.Truthy() {
		return 0, "", false
	}
	var d struct {
		Cnt  int    `json:"cnt"`
		Txt  string `json:"txt"`
		Stop bool   `json:"stop"`
	}
	if err := json.Unmarshal([]byte(raw.Str2()), &d); err != nil {
		return 0, "", false
	}
	return d.Cnt, strings.TrimSpace(d.Txt), d.Stop
}

// ChatStream 对应 chat_stream()。
//
// DEVIATION: Python 是 generator，Go 没有等价的惰性迭代，改成回调式：
// onDelta 每收到一段增量文字就被调用一次；返回 error 对应 Python 的 raise。
// 调用顺序、CDP 报文与超时判定与 Python 完全一致。
func (e *Engine) ChatStream(cookies map[string]string, prompt string, expires map[string]int,
	timeout int, accountID string, stop <-chan struct{}, onDelta func(string) error) error {

	if timeout <= 0 {
		timeout = e.CFG.ChatTimeout
	}
	if _, err := e.ensurePage(cookies, expires, accountID); err != nil {
		return err
	}
	e.ResetThread(true)
	baseAgent, baseText, _ := e.pollChat()
	if _, err := e.send(prompt); err != nil {
		return err
	}

	tSent := e.now()
	deadline := tSent.Add(time.Duration(timeout) * time.Second)
	firstTokenDeadline := tSent.Add(40 * time.Second)
	if deadline.Before(firstTokenDeadline) {
		firstTokenDeadline = deadline
	}

	sent, last := "", ""
	stable := 0
	gotFirst := false

	// 1. 等助手开始吐字：60ms 高频采样，捕到首批增量就立刻交出去
	for e.now().Before(firstTokenDeadline) {
		if aborted(stop) {
			return nil
		}
		e.sleep(60 * time.Millisecond)
		cnt, cur, _ := e.pollChat()
		if cur == "" || (cnt <= baseAgent && cur == baseText) {
			if e.now().Sub(tSent) > 14*time.Second {
				tail := ""
				if v, err := e.page.JS(jsChatStreamL893, false, 0); err == nil {
					tail = v.Str2()
				}
				if strings.Contains(tail, "Still sending") || strings.Contains(tail, "Connecting...") {
					return &ErrGeneration{Msg: "云端 VM 连接超时 (Still sending)"}
				}
			}
			continue
		}
		delta := cur
		if strings.HasPrefix(cur, sent) {
			delta = cur[len(sent):]
		}
		if delta != "" {
			sent = cur
			if err := onDelta(delta); err != nil {
				return err
			}
			last = cur
			gotFirst = true
			break
		}
	}
	if !gotFirst {
		return &ErrGeneration{Msg: "等待助手首字响应超时"}
	}

	// 2. 持续捕获增量文本
	for e.now().Before(deadline) {
		if aborted(stop) {
			return nil
		}
		e.sleep(60 * time.Millisecond)
		_, cur, hasStop := e.pollChat()
		if cur == "" {
			continue
		}
		if cur != last {
			delta := cur
			if strings.HasPrefix(cur, sent) {
				delta = cur[len(sent):]
			}
			if delta != "" {
				sent = cur
				if err := onDelta(delta); err != nil {
					return err
				}
			}
			last, stable = cur, 0
		} else {
			stable++
			// Stop 按钮消失 = 前端生成彻底结束，连续 3 次（约 0.18s）无新文本即退出；
			// Stop 还在时绝不过早截断（思考/代码块/网络抖动），放宽到 80 次（约 5s）防卡死。
			if !hasStop && stable >= 3 {
				return nil
			}
			if hasStop && stable >= 80 {
				return nil
			}
		}
	}
	return &ErrGeneration{Msg: "等待助手回复超时"}
}

// Chat 对应 chat()：非流式，把增量拼成完整回复。
func (e *Engine) Chat(cookies map[string]string, prompt string, expires map[string]int,
	timeout int, accountID string) (string, error) {
	var b strings.Builder
	err := e.ChatStream(cookies, prompt, expires, timeout, accountID, nil, func(s string) error {
		b.WriteString(s)
		return nil
	})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

func aborted(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}
