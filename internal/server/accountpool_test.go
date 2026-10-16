package server

import (
	"testing"
	"time"
)

func addLiveAccount(a *App, label string) string {
	acc := a.Store.AddAccount(map[string]any{
		"hatch_sess": "s", "hatch_gw": "g", "hatch_vml": "v",
		"hatch_native_auth_device": "d"}, label, nil)
	return strOf(acc["id"])
}

// 账号池的并发契约：两个账号可同时各租一个；全忙时第三个任务等待超时；
// 释放后等待者立刻拿到（对应上游 AccountPoolManager 的 Condition.notify_all）。
func TestAccountPoolExclusivePerAccount(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	addLiveAccount(a, "甲")
	addLiveAccount(a, "乙")

	first, err := a.accounts.acquire("", "", true, false, time.Second, true)
	if err != nil || first == nil {
		t.Fatalf("第一个任务应拿到账号: %v %v", first, err)
	}
	second, err := a.accounts.acquire("", "", true, false, time.Second, true)
	if err != nil || second == nil {
		t.Fatalf("第二个任务应拿到另一个账号: %v %v", second, err)
	}
	if strOf(first["id"]) == strOf(second["id"]) {
		t.Fatalf("两个任务拿到了同一个账号: %v", first["id"])
	}
	if _, err := a.accounts.acquire("", "", true, false, 60*time.Millisecond, true); err == nil {
		t.Fatal("所有账号都繁忙时应等待超时并报错")
	}

	released := make(chan map[string]any, 1)
	go func() {
		acc, err := a.accounts.acquire("", "", true, false, 2*time.Second, true)
		if err != nil {
			released <- nil
			return
		}
		released <- acc
	}()
	time.Sleep(30 * time.Millisecond)
	a.accounts.release(strOf(first["id"]))
	select {
	case acc := <-released:
		if acc == nil || strOf(acc["id"]) != strOf(first["id"]) {
			t.Fatalf("释放后等待者应拿到刚释放的账号，得到 %v", acc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("释放后等待者没有被唤醒")
	}
}

// 指定账号被占用/禁用时的两种错误语义。
func TestAccountPoolSpecificAccountErrors(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	aid := addLiveAccount(a, "甲")

	if _, err := a.accounts.acquire(aid, "", true, false, time.Second, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.accounts.acquire(aid, "", true, false, 60*time.Millisecond, true); err == nil {
		t.Fatal("同一个指定账号被占用时应等待超时")
	}
	a.accounts.release(aid)

	a.Store.UpdateAccount(aid, map[string]any{"enabled": false})
	if _, err := a.accounts.acquire(aid, "", true, false, time.Second, true); err == nil {
		t.Fatal("禁用的指定账号应报错")
	}
}

// record_use=false：预热租约不改账号轮换计数。
func TestAccountPoolRecordUseFalse(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	aid := addLiveAccount(a, "甲")
	before := numOr(a.Store.GetAccount(aid)["use_count"])

	acc, err := a.accounts.acquire("", "", false, false, time.Second, false)
	if err != nil || acc == nil {
		t.Fatalf("应租到账号: %v %v", acc, err)
	}
	if got := numOr(a.Store.GetAccount(aid)["use_count"]); got != before {
		t.Fatalf("record_use=false 不该增加 use_count: before=%v after=%v", before, got)
	}
}

// 并发锁容量跟随 MUSE2API_CONCURRENCY：容量 2 时两个任务同时在跑，第三个被挡住。
func TestBrowserLockCapacityFollowsConcurrency(t *testing.T) {
	b := newBrowserLock(2)
	release := make(chan struct{})
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_ = b.runBlocking(nil, func() { <-release })
			done <- struct{}{}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(b.ch) < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(b.ch) != 2 {
		t.Fatalf("两个槽位没被占满: %d", len(b.ch))
	}
	if b.tryRun(func() { t.Error("第三个任务不该拿到槽位") }) {
		t.Error("两个槽位都占着时 tryRun 不该成功")
	}
	close(release)
	<-done
	<-done
	if b.locked() {
		t.Error("全部释放后锁不该仍是占用")
	}
}
