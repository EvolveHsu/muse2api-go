package engine

import (
	"sync"
	"testing"
	"time"

	"muse2api-go/internal/config"
)

// tickProc 是并发测试用的假进程：只关心 Alive() 能被安全地并发调用。
type tickProc struct{}

func (tickProc) Alive() bool { return true }
func (tickProc) Terminate()  {}
func (tickProc) Kill()       {}

// 装好真实进程之后，一个浏览器实例会被三条线同时碰：
//
//	① 面板每几秒打 /readyz 与 /admin/status（BrowserProcUp，不抢锁也不该等）；
//	② 保活协程在 GEN_LOCK **外**读写 lastHTTPRenew 那张表；
//	③ 生成主链路在 GEN_LOCK 内换 page/proc/currentAccID。
//
// 快照只保证「一个写者 vs 多个锁外读者」的安全，写者之间的互斥仍旧由 GEN_LOCK 负责 ——
// 这正是 /admin/relogin 必须改成排队的理由（见 server/keepalive.go 里那条 DEVIATION）。
// 修复前这条测试跑不到半秒就崩：那张表被并发写是 `fatal error: concurrent map writes`，
// 接口值被并发读写则炸成「value method tickProc.Alive called using nil *tickProc pointer」。
func TestSharedStateUnderConcurrentReaders(t *testing.T) {
	e := NewEngine(&config.Config{}, nil, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	loop := func(body func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					body()
				}
			}
		}()
	}

	for i := 0; i < 6; i++ {
		loop(func() {
			_ = e.BrowserProcUp()
			_ = e.Page()
			_ = e.CurrentAccountID()
			_ = e.LastHTTPRenew("acc-1")
		})
	}
	// 保活协程与主链路共用那张续签表，而它自己不抢浏览器锁。
	for i := 0; i < 3; i++ {
		loop(func() { e.setRenewAt("acc-2", float64(time.Now().Unix())) })
	}
	for i := 0; i < 3; i++ {
		loop(func() { _ = e.LastHTTPRenew("acc-2") })
	}
	// 唯一写者：模拟 GEN_LOCK 的持有者在换状态。
	loop(func() {
		e.setProc(tickProc{})
		e.setAccountID("acc-1")
		e.setProc(nil)
		e.setPage(nil)
		e.setAccountID("")
	})

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 收尾两条不变量：空状态读出来必须是空；表里的值可读。
	e.setProc(nil)
	e.setPage(nil)
	e.setAccountID("")
	if e.BrowserProcUp() || e.Page() != nil || e.CurrentAccountID() != "" {
		t.Error("快照没跟着状态清空")
	}
	e.setRenewAt("acc-9", 42)
	if got := e.LastHTTPRenew("acc-9"); got != 42 {
		t.Errorf("续签表读写不通：%v", got)
	}
}

// publish 之前，锁外的读者不该看到半截变更；publish 之后必须一次看到整组新值。
func TestPublishIsWholeSnapshot(t *testing.T) {
	e := NewEngine(&config.Config{}, nil, nil)
	if e.BrowserProcUp() || e.CurrentAccountID() != "" {
		t.Error("新建引擎的快照不该是满的")
	}
	e.proc = tickProc{}
	e.currentAccID = "acc-1"
	if e.BrowserProcUp() {
		t.Error("没 publish 之前，锁外读者不该看到变更")
	}
	e.publish()
	if !e.BrowserProcUp() || e.CurrentAccountID() != "acc-1" {
		t.Error("publish 之后读者应立刻看到整组新值")
	}
}
