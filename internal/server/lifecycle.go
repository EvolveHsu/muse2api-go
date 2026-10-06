package server

// 进程生命周期：对应 app.py 的 @app.on_event("startup") / ("shutdown") 与 _keepalive_loop()。
// Python 把这几件事挂在 lifespan 钩子上，Go 没有等价的框架钩子，改由 cmd/muse2api 显式调用
// Startup / Shutdown，循环本身（顺序、周期、异常处理）逐句照搬 _keepalive_loop。

import (
	"log"
	"time"
)

// Startup 做三件事，顺序与 Python 一致：
//
//	① 被重启打断的生图任务判失败（queued/processing 永远等不到结果）；
//	② 没配 MUSE2API_KEY 就自动生成一把初始密钥并写回 .env；
//	③ 拉起后台保活守护任务。
func (a *App) Startup(stop <-chan struct{}) {
	if n := a.Store.FailInterruptedTasks("服务重启中断了任务，请重新提交"); n > 0 {
		log.Printf("【启动】%d 个被重启打断的生图任务已置为失败", n)
	}
	if a.apiKey() == "" {
		newKey := "m2a_" + randHex(24) // 与 secrets.token_hex(24) 同宽：48 位十六进制
		a.mu.Lock()
		a.CFG.APIKey = newKey
		a.mu.Unlock()
		a.persistEnv("MUSE2API_KEY", newKey)
		log.Printf("🔑 未检测到 MUSE2API_KEY，已自动生成初始密钥: %s", newKey)
	}
	go a.keepaliveLoop(stop)
	// DEVIATION: Python 不释放浏览器，也就没有这个回收器；
	// Go 为了 <100MB 的空闲口径新增，MUSE2API_WARM_TAB=1 或把分钟数设成 0 就退回 Python 的行为。
	if !a.CFG.WarmTab && a.CFG.BrowserIdleMinutes > 0 {
		go a.idleEvictor(stop)
	}
}

// Shutdown 对应 @app.on_event("shutdown")：只有一句 engine.stop()。
func (a *App) Shutdown() { a.Engine.Stop() }

// keepaliveLoop 对应 _keepalive_loop()：先睡 1 秒再预热一次，之后每 900 秒一轮。
// 每一轮 Python 是「保活 → 热标签页没了就复活」；Go 默认**只做纯 HTTP 保活**，
// 复活那半段挂在 MUSE2API_WARM_TAB 上（这是全项目唯一一处有意新增的行为，见设计文档 §3）。
func (a *App) keepaliveLoop(stop <-chan struct{}) {
	log.Printf("【自动保活守护进程】已启动，检测周期: 15 分钟")
	if !sleepOrStop(time.Second, stop) {
		return
	}
	a.warmupBrowser()
	for {
		// Python 整段包在 try/except 里：单轮出错只记日志，守护任务不能因此退出。
		a.safeRound(func() {
			a.runKeepaliveAll(false)
			if a.CFG.WarmTab && (a.Engine.CurrentAccountID() == "" || a.Engine.Page() == nil) {
				a.warmupBrowser()
			}
		})
		if !sleepOrStop(900*time.Second, stop) {
			return
		}
	}
}

// warmupBrowser 对应 _warmup_browser_sync()：抢不到浏览器锁就直接放弃（不排队），
// 预热用的也是非阻塞语义，绝不能把在跑的任务挤掉。
func (a *App) warmupBrowser() {
	if !a.CFG.WarmTab {
		return
	}
	if a.Engine.CurrentAccountID() != "" && a.Engine.Page() != nil {
		return
	}
	acc := a.Store.PickAccount("", false, false, "")
	if acc == nil || !truthy(acc["cookies"]) {
		return
	}
	accID := strOf(acc["id"])
	label := strOf(acc["label"])
	a.browser.tryRun(func() {
		log.Printf("【浏览器预热】正在后台预热账号 %s (%s) 的热备标签页...", label, accID)
		refreshed, err := a.renewAndPersist(accID, true, false)
		if err != nil {
			log.Printf("【浏览器预热】续签异常: %v", err)
			return
		}
		if refreshed != nil {
			acc = refreshed
		}
		if err := a.Engine.Start(); err != nil {
			log.Printf("【浏览器预热】启动浏览器失败: %v", err)
			return
		}
		if _, err := a.Engine.EnsurePage(toStringMap(acc["cookies"]),
			cookieIntMap(acc["cookies_exp"]), accID); err != nil {
			log.Printf("【浏览器预热】预热异常: %v", err)
			return
		}
		log.Printf("【浏览器预热】账号 %s (%s) 热备标签页与 WebSocket 已就绪", label, accID)
	})
}

// idleEvictor 是空闲回收：连续 BrowserIdleMinutes 分钟没碰过浏览器、锁也空着，
// 就把整个 Chromium 进程树停掉（这是那 465MB 的唯一来源）。
// 真正动手前用 tryRun 再抢一次锁：万一检查和关停之间来了任务，抢不到就什么都不做。
func (a *App) idleEvictor(stop <-chan struct{}) {
	idle := time.Duration(a.CFG.BrowserIdleMinutes) * time.Minute
	log.Printf("【空闲回收】浏览器静默 %v 后释放（MUSE2API_BROWSER_IDLE_MINUTES）", idle)
	for {
		if !sleepOrStop(time.Minute, stop) {
			return
		}
		quiet := a.browser.idleFor()
		if !shouldEvict(a.browser.busy(), a.Engine.BrowserProcUp(), quiet, idle) {
			continue
		}
		// tryRun 在放开锁时自己会 touch 一次，所以下一轮至少要再等满 idle 才会再关一遍。
		a.browser.tryRun(func() {
			if a.Engine.BrowserProcUp() {
				// 静默时长按抢锁之前的读数报：tryRun 一拿到锁就会刷新它，抢完再读永远是 0s。
				log.Printf("【空闲回收】已静默 %v，关闭浏览器释放内存（下条请求会冷启动）",
					quiet.Truncate(time.Second))
				a.Engine.Stop()
			}
		})
	}
}

// shouldEvict 是空闲回收的判定式。单独抽出来是因为「浏览器正在跑」这个前提
// 在测试里造不出来（得真起一个 Chromium 进程），而这条判定的对错直接关系到
// 会不会把别人正在用的浏览器关掉 —— 那是唯一一处不能出错的新增行为。
func shouldEvict(busy, procUp bool, idleFor, idle time.Duration) bool {
	return !busy && procUp && idleFor >= idle
}

// safeRound 对应 _keepalive_loop 里那圈 try/except。
func (a *App) safeRound(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("【自动保活守护进程】轮询异常: %v", r)
		}
	}()
	fn()
}

func sleepOrStop(d time.Duration, stop <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}
