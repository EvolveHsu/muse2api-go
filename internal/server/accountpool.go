package server

import (
	"sync"
	"time"

	"muse2api-go/internal/engine"
	"muse2api-go/internal/store"
)

// accountPool 对应上游 1.6.0 的 AccountPoolManager：
// 同一个账号同一时刻只能被一个并发任务持有；多账号时任务并行执行。
// Python 用 Condition + busy_accounts 集合实现，Go 用「集合 + 换代通知 channel」等价复刻。
type accountPool struct {
	store *store.Store

	mu   sync.Mutex
	busy map[string]bool
	// wait 在每次 release 时关闭并换新；等待者 select 它或 0.5 秒定时器，
	// 与 Python 的 cond.wait(timeout=min(remaining, 0.5)) 轮询语义一致。
	wait chan struct{}
}

func newAccountPool(st *store.Store) *accountPool {
	return &accountPool{store: st, busy: map[string]bool{}, wait: make(chan struct{})}
}

// acquire 对应 AccountPoolManager.acquire()：等到一个空闲账号（或指定账号空闲）为止。
func (p *accountPool) acquire(accountID, preferredID string, rotate, forceRotate bool,
	timeout time.Duration, recordUse bool) (map[string]any, error) {

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &engine.ErrGeneration{Msg: "等待空闲账号超时，当前所有账号均处于繁忙中"}
		}

		p.mu.Lock()
		acc, waitCh, err := p.tryLocked(accountID, preferredID, rotate, forceRotate, recordUse)
		p.mu.Unlock()
		if acc != nil {
			return acc, nil
		}
		if err != nil {
			return nil, err
		}

		tick := remaining
		if tick > 500*time.Millisecond {
			tick = 500 * time.Millisecond
		}
		timer := time.NewTimer(tick)
		select {
		case <-waitCh:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// tryLocked 是 acquire 的单轮尝试；p.mu 必须已被持有。
func (p *accountPool) tryLocked(accountID, preferredID string, rotate, forceRotate bool,
	recordUse bool) (map[string]any, <-chan struct{}, error) {

	if accountID != "" {
		acc := p.store.GetAccount(accountID)
		if acc == nil || !accountEnabled(acc) {
			return nil, nil, &engine.ErrAuth{Msg: "指定账号 " + accountID + " 不存在或已禁用"}
		}
		if !p.busy[accountID] {
			p.busy[accountID] = true
			return acc, nil, nil
		}
	} else {
		pref := preferredID
		if pref != "" && p.busy[pref] {
			pref = ""
		}
		busyIDs := make([]string, 0, len(p.busy))
		for id := range p.busy {
			busyIDs = append(busyIDs, id)
		}
		// 注意：busyIDs 永远非 nil —— 对应上游给 pick_account 传的 exclude_ids=空 set，
		// 「所有账号都繁忙」时必须返回 None 去等待，而不是回退到繁忙账号。
		if acc := p.store.PickAccount(pref, rotate, forceRotate, "", busyIDs, recordUse); acc != nil {
			p.busy[strOf(acc["id"])] = true
			return acc, nil, nil
		}
	}

	if !p.store.HasLiveAccounts() {
		return nil, nil, &engine.ErrAuth{Msg: "没有可用账号，请先在管理页面导入 cookie"}
	}
	return nil, p.wait, nil
}

// release 对应 AccountPoolManager.release()：放开账号并唤醒所有等待者。
func (p *accountPool) release(accountID string) {
	if accountID == "" {
		return
	}
	p.mu.Lock()
	if p.busy[accountID] {
		delete(p.busy, accountID)
	}
	old := p.wait
	p.wait = make(chan struct{})
	p.mu.Unlock()
	close(old)
}

// accountEnabled 对应 store 侧 `a.get("enabled", True)` 的判定（None/缺省都算启用）。
func accountEnabled(a map[string]any) bool {
	v, ok := a["enabled"]
	if !ok || v == nil {
		return true
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return truthy(v)
}

// okIsFalse 对应 Python 的 `x is False`（None/缺省不算 False）。
func okIsFalse(v any) bool {
	b, ok := v.(bool)
	return ok && !b
}
