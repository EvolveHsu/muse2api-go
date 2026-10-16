package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Startup 的第一件事：把被重启打断的生图/生视频任务判失败（上游 1.6.0 起视频也纳入清理）。
func TestStartupFailsInterruptedTasks(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	img := a.Store.CreateTask("image", "猫一")
	processing := a.Store.CreateTask("image", "猫二")
	done := a.Store.CreateTask("image", "猫三")
	vid := a.Store.CreateTask("video", "片一")
	a.Store.UpdateTask(processing["id"].(string), map[string]any{"status": "processing"})
	a.Store.UpdateTask(done["id"].(string), map[string]any{"status": "completed"})

	stop := make(chan struct{})
	defer close(stop)
	a.Startup(stop)

	for _, tid := range []string{img["id"].(string), processing["id"].(string), vid["id"].(string)} {
		task := a.Store.GetTask(tid)
		if task["status"] != "failed" || task["error"] != "服务重启中断了任务，请重新提交" {
			t.Errorf("任务 %s 没被打断标记：%v / %v", tid, task["status"], task["error"])
		}
	}
	if got := a.Store.GetTask(done["id"].(string)); got["status"] != "completed" {
		t.Errorf("已完成的任务被改了：%v", got["status"])
	}
	// 落盘也得跟上，不然重启读回来的还是旧状态。
	raw, err := os.ReadFile(filepath.Join(a.CFG.DataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "服务重启中断了任务"); n != 3 {
		t.Errorf("落盘里应有 3 条打断说明，实得 %d：%s", n, raw)
	}
}

// 没配 MUSE2API_KEY 时自动生成一把并写回 .env（Python  startup 的同一段）。
func TestStartupGeneratesMissingAPIKey(t *testing.T) {
	a, _ := testApp(t, "", time.Unix(1700000000, 0))
	stop := make(chan struct{})
	defer close(stop)
	a.Startup(stop)

	key := a.CFG.APIKey
	if !strings.HasPrefix(key, "m2a_") || len(key) != len("m2a_")+48 {
		t.Fatalf("生成的密钥形状不对（应为 m2a_ + 48 位十六进制）：%q", key)
	}
	raw, err := os.ReadFile(filepath.Join(a.CFG.BaseDir, ".env"))
	if err != nil {
		t.Fatalf(".env 没写回去: %v", err)
	}
	if !strings.Contains(string(raw), "MUSE2API_KEY="+key) {
		t.Errorf(".env 里没有新密钥：%q", string(raw))
	}
	// 新密钥必须立刻能用：/v1/models 带它应 200，不带应 401。
	if rec := get(a, "/v1/models", key); rec.Code != 200 {
		t.Errorf("用新密钥访问 /v1/models go=%d", rec.Code)
	}
	if rec := get(a, "/v1/models", ""); rec.Code != 401 {
		t.Errorf("不带密钥访问 /v1/models go=%d，应 401", rec.Code)
	}
}

func TestBrowserLockTracksActivity(t *testing.T) {
	b := newBrowserLock(1)
	if b.busy() {
		t.Fatal("新建的锁不该被占着")
	}
	// 假装浏览器一小时以前用过，然后走一次带锁的任务：拿到锁就要把静默期清零。
	b.last = time.Now().Add(-time.Hour)
	if err := b.run(time.Second, func() error {
		if !b.busy() {
			t.Error("任务体里锁应显示被占用")
		}
		if b.idleFor() > time.Minute {
			t.Errorf("拿到锁后静默期没刷新：%v", b.idleFor())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if b.busy() {
		t.Error("任务结束后锁应已释放")
	}
	if b.idleFor() > time.Minute {
		t.Errorf("释放时也该刷新静默期，实得 %v", b.idleFor())
	}

	// 超时那条路（等待队列超时）没有拿到锁，不能刷新静默期。
	b.last = time.Now().Add(-time.Hour)
	b.ch <- struct{}{}
	if err := b.run(20*time.Millisecond, func() error { return nil }); err == nil {
		t.Error("锁被占着时 run 应超时")
	}
	if b.idleFor() < time.Hour {
		t.Error("没拿到锁就不该刷新静默期")
	}
	<-b.ch
}

func TestBrowserLockTryRunIsNonBlocking(t *testing.T) {
	b := newBrowserLock(1)
	inside := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = b.runBlocking(nil, func() {
			close(inside)
			<-done
		})
	}()
	<-inside
	if b.tryRun(func() { t.Error("抢不到锁时不该执行任务体") }) {
		t.Error("tryRun 在锁被占用时应立刻返回 false")
	}
	close(done)
	// 释放之后才抢得到。
	for i := 0; i < 500; i++ {
		if ok := b.tryRun(func() {}); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Error("锁释放后 tryRun 仍抢不到")
}

func TestShouldEvict(t *testing.T) {
	idle := 30 * time.Minute
	cases := []struct {
		why     string
		busy    bool
		procUp  bool
		idleFor time.Duration
		want    bool
	}{
		{"正常空闲", false, true, idle, true},
		{"还没到点", false, true, idle - time.Second, false},
		{"有人在用", true, true, time.Hour, false},
		{"浏览器本来就没人起过", false, false, time.Hour, false},
	}
	for _, c := range cases {
		if got := shouldEvict(c.busy, c.procUp, c.idleFor, idle); got != c.want {
			t.Errorf("%s：shouldEvict go=%v want=%v", c.why, got, c.want)
		}
	}
}

// safeRound 对应 _keepalive_loop 里那圈 try/except：单轮出错只记日志，守护任务不能因此退出。
func TestSafeRoundRecovers(t *testing.T) {
	ran := false
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	a.safeRound(func() {
		ran = true
		panic("模拟保活轮次里的异常")
	})
	if !ran {
		t.Error("任务体没被执行")
	}
	// 走到这里没崩就说明异常被吞下了，与 Python 的 except 分支等价。
}

// 预热在没有账号时必须在动浏览器之前就返回 —— 这条是「测试不烧额度、不起浏览器」纪律的关口。
func TestWarmupWithoutAccounts(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	a.CFG.WarmTab = true
	a.warmupBrowser()
	if a.Engine.BrowserProcUp() {
		t.Error("没有账号时不该把浏览器起来")
	}
}

// 默认配置（WarmTab=false）下 keepaliveLoop 只跑纯 HTTP 保活，不该碰浏览器。
// 空账号池时 runKeepaliveAll 自己就返回了，这里验的是「循环能起来、也不会起浏览器」。
func TestKeepaliveLoopDoesNotWarmByDefault(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	stop := make(chan struct{})
	go a.keepaliveLoop(stop)
	time.Sleep(1200 * time.Millisecond) // 越过循环开头那 1 秒
	close(stop)
	// 循环退出的耗时没有上界保证：原来这里睡死 100 毫秒，机器满载时（实测：紧跟在三次
	// 交叉编译之后跑全量）会偶发误红。改成轮询到条件成立、上限 5 秒 —— 真的留着锁照样红。
	quit := time.Now().Add(5 * time.Second)
	for a.browser.busy() && time.Now().Before(quit) {
		time.Sleep(20 * time.Millisecond)
	}
	if a.Engine.BrowserProcUp() {
		t.Error("默认策略下保活循环不该预热浏览器")
	}
	if a.browser.busy() {
		t.Error("保活循环结束时不该留着锁")
	}
}
