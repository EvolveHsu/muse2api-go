package server

import (
	"errors"
	"strings"

	"muse2api-go/internal/engine"
)

// streamItem 是 safe_chat_stream 往外吐的一条：文本增量或错误。
// Python 用 ("data"/"error"/"done", value) 三元组，Go 里关闭通道就等价于 done。
type streamItem struct {
	Text string
	Err  error
}

func stopped(stop <-chan struct{}) bool {
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

// safeChatStream 对应 app.py 的 safe_chat_stream()。
//
// 照抄的三条约束：生成在独立 goroutine 里跑并全程持有浏览器锁，主侧从带缓冲通道取；
// 客户端无论何时断连（或异常/超时），stop + defer 保证锁 100% 立即释放，绝不死锁；
// 首字之前遇到单账号 VM 卡死或会话异常，自动切下一个健康账号重试一次。
func (a *App) safeChatStream(accID string, cookies map[string]string,
	expires map[string]int, prompt string, timeout int, stop <-chan struct{}) <-chan streamItem {

	out := make(chan streamItem, 100) // Queue(maxsize=100)
	go func() {
		defer close(out)
		curID, curCookies, curExp := accID, cookies, expires
		var lastErr error
		for attempt := 0; attempt < 2; attempt++ {
			if stopped(stop) {
				return
			}
			if attempt > 0 {
				alt := a.Store.PickAccount("", true, true, curID)
				if alt == nil || strOf(alt["id"]) == curID {
					break
				}
				curID = strOf(alt["id"])
				curCookies = toStringMap(alt["cookies"])
				curExp = cookieIntMap(alt["cookies_exp"])
			}
			yielded := false
			err := a.attemptChat(curID, curCookies, curExp, prompt, timeout, stop,
				&yielded, out, attempt > 0)
			if err == nil {
				return
			}
			lastErr = err
			var ae *engine.ErrAuth
			if errors.As(err, &ae) {
				if curID != "" {
					a.Store.Mark(curID, false, ae.Error())
				}
				if yielded {
					break
				}
				continue
			}
			a.Engine.ResetThread(false)
			if yielded {
				break
			}
		}
		if lastErr != nil {
			out <- streamItem{Err: lastErr}
		}
	}()
	return out
}

// attemptChat 对应 worker 里那层 try：续签 → 抢锁 → 起浏览器 → 流式吐字 → 记成功并同步 cookie。
// 续签失败不吞：Python 没有包它，MuseAuthError 会走到 except MuseAuthError 那一支。
func (a *App) attemptChat(accID string, cookies map[string]string, expires map[string]int,
	prompt string, timeout int, stop <-chan struct{}, yielded *bool,
	out chan<- streamItem, force bool) error {

	if accID != "" {
		refreshed, err := a.renewAndPersist(accID, true, force)
		if err != nil {
			return err
		}
		if refreshed != nil {
			cookies = toStringMap(refreshed["cookies"])
			expires = cookieIntMap(refreshed["cookies_exp"])
		}
	}
	var runErr error
	if err := a.browser.runBlocking(stop, func() {
		if stopped(stop) {
			return
		}
		if err := a.Engine.Start(); err != nil {
			runErr = err
			return
		}
		runErr = a.Engine.ChatStream(cookies, prompt, expires, timeout, accID, stop,
			func(chunk string) error {
				*yielded = true
				out <- streamItem{Text: chunk}
				if stopped(stop) {
					return errStopped
				}
				return nil
			})
	}); err != nil {
		return err // 客户端断开：锁已由 defer 释放
	}
	if runErr == nil && accID != "" {
		a.Store.Mark(accID, true, "")
		a.syncCookies(accID)
	}
	return runErr
}

// collectChat 把流式结果拼成整段文本，对应非流式分支里的 "".join(...)。
func collectChat(items <-chan streamItem) (string, error) {
	var b strings.Builder
	for it := range items {
		if it.Err != nil {
			return "", it.Err
		}
		b.WriteString(it.Text)
	}
	return b.String(), nil
}
