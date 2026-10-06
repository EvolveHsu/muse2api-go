package server

import (
	"errors"
	"strings"
	"time"

	"muse2api-go/internal/engine"
	"muse2api-go/internal/pyval"
)

// keepalive.go 对应 app.py 的「账号自动保活与静默续期」加几条要抢浏览器锁的管理路由。
//
// v1.5.3 的核心修复必须保住：只有 401 才判 cookie 失效。
// 403/429/5xx/超时/非 JSON/未 assigned 只记诊断信息（touch_keepalive 传 ok=nil），
// 绝不覆盖上一次已确认的可用状态 —— 这条由 TestProbeAccountSync 逐分支钉住。

func (a *App) registerBrowserAdmin() {
	a.add("POST", "/admin/accounts/{aid}/test", true, a.testAccount)
	a.add("POST", "/admin/accounts/{aid}/relogin", true, a.testAccount)
	a.add("POST", "/admin/accounts/{aid}/quota", true, a.queryQuota)
	a.add("POST", "/admin/quota", true, a.queryAnyQuota)
	a.add("POST", "/admin/relogin", true, a.reloginAll)
	a.add("POST", "/admin/accounts/keepalive", true, a.keepaliveAll)
	a.add("POST", "/admin/accounts/{aid}/keepalive", true, a.keepaliveSingle)
}

// --- 单账号探测（纯 HTTP，不开浏览器） ---

// probeAccountSync 对应 _probe_account_sync()：
// 打一次 /api/session 让网关签新 hatch_vml(+48h)/hatch_sess(+30d)，并唤醒云端 VM。
func (a *App) probeAccountSync(aid string, checkQuota bool) pyval.Obj {
	acc := a.Store.GetAccount(aid)
	label := aid
	if acc != nil {
		label = strOf(acc["label"])
		if label == "" {
			label = aid
		}
	}
	if acc == nil || !truthy(acc["cookies"]) {
		return pyval.Obj{{"ok", false}, {"id", aid}, {"label", label},
			{"error", "账号无有效 cookie"}}
	}

	res, err := a.Engine.RenewSessionHTTP(toStringMap(acc["cookies"]),
		cookieIntMap(acc["cookies_exp"]), true)
	if err != nil {
		var ae *engine.ErrAuth
		if errors.As(err, &ae) {
			a.Store.TouchKeepalive(aid, boolPtrT(false),
				"保活认证失败: "+truncate(ae.Error(), 200))
			return pyval.Obj{{"ok", false}, {"id", aid}, {"label", label},
				{"error", ae.Error()}}
		}
		a.Store.TouchKeepalive(aid, nil,
			"保活未确认（保留账号状态）: "+truncate(err.Error(), 200))
		return pyval.Obj{{"ok", false}, {"id", aid}, {"label", label},
			{"error", err.Error()}}
	}

	okVal := false
	if res.OK {
		okVal = true
	}
	// store 里存的是 JSON 形状（map[string]any），直接把 map[string]string / map[string]int
	// 塞进去会让后续所有 `.(map[string]any)` 断言静默落空 —— cookie 与有效期都会「存了但读不到」。
	a.Store.UpdateAccount(aid, map[string]any{
		"cookies": toStringAny(res.Cookies), "cookies_exp": toIntAny(res.CookiesExp),
		"ok": okVal, "synced_at": a.now().Unix(),
	})
	vmState := res.VMState
	if vmState == "" {
		vmState = "RUNNING"
	}
	a.Store.TouchKeepalive(aid, boolPtrT(true), "会话有效 · 自动保活 (VM: "+vmState+")")

	quota := acc["quota"]
	if checkQuota {
		if locked := a.browser.tryRun(func() {
			if err := a.Engine.Start(); err != nil {
				return
			}
			q, qerr := a.Engine.Quota(res.Cookies, res.CookiesExp)
			if qerr != nil {
				return
			}
			q["checked_at"] = a.now().Unix()
			a.Store.UpdateAccount(aid, map[string]any{"quota": q})
			quota = q
		}); !locked {
			// 抢不到锁就跳过额度，不排队：保活不该被生成堵住
			_ = locked
		}
	}
	updated := a.Store.GetAccount(aid)
	expiresAt := any(nil)
	if updated != nil {
		expiresAt = updated["expires_at"]
	}
	return pyval.Obj{
		{"ok", true}, {"id", aid}, {"label", label},
		{"vm_id", res.VMID}, {"vm_state", vmState}, {"wake_ok", res.WakeOK},
		{"expires_at", expiresAt}, {"quota", quota},
	}
}

// --- 全账号保活 ---

func (a *App) runKeepaliveAll(force bool) pyval.Obj {
	a.kaMu.Lock()
	defer a.kaMu.Unlock()
	now := a.now().Unix()
	a.kaState.Running = true
	a.kaState.LastRun = now
	defer func() { a.kaState.Running = false }()

	var results, skipped []any
	includeDisabled := a.CFG.KeepaliveDisabledAccounts
	for _, acc := range a.Store.ListAccounts() {
		if enabled, ok := acc["enabled"].(bool); (!ok || enabled) || includeDisabled {
			aid := strOf(acc["id"])
			label := strOf(acc["label"])
			if label == "" {
				label = aid
			}
			hasExp := acc["expires_at"] != nil
			expAt := int64(numOr(acc["expires_at"]))
			lastKA := int64(numOr(acc["last_keepalive"]))
			okTrue := acc["ok"] == true
			needsRun := force || !hasExp || (expAt-now < 36*3600) ||
				(now-lastKA > 15*60) || !okTrue
			if !needsRun {
				skipped = append(skipped, pyval.Obj{{"id", aid}, {"label", label},
					{"reason", "会话充足且近期已保活"}})
				continue
			}
			res := a.probeAccountSync(aid, false)
			results = append(results, res)
			time.Sleep(500 * time.Millisecond)
		}
	}
	summary := pyval.Obj{
		{"checked_at", now}, {"refreshed_count", len(results)}, {"refreshed", results},
		{"skipped_count", len(skipped)}, {"skipped", skipped},
	}
	a.kaState.LastResult = summary
	a.kaState.NextRun = now + 900
	return summary
}

func (a *App) keepaliveAll(c *reqCtx) (any, error) {
	force := true
	if v := c.R.URL.Query().Get("force"); v != "" {
		force = v != "0" && !strings.EqualFold(v, "false")
	}
	a.kaMu.Lock()
	running := a.kaState.Running
	a.kaMu.Unlock()
	if running {
		return pyval.Obj{{"status", "busy"},
			{"message", "保活任务正在执行中，请稍候"}}, nil
	}
	return a.runKeepaliveAll(force), nil
}

func (a *App) keepaliveSingle(c *reqCtx) (any, error) {
	return a.probeAccountSync(c.Args["aid"], false), nil
}

// --- 要抢浏览器锁的三条：会话测试 / 额度 / 重登 ---

// testAccount 对应 test_account()：真实打开 muse.ai 验证 cookie 是否仍可登录。
// 失败也回 200（面板按 ok 字段渲染，状态码不区分）。
func (a *App) testAccount(c *reqCtx) (any, error) {
	aid := c.Args["aid"]
	acc := a.Store.GetAccount(aid)
	if acc == nil {
		return nil, httpError(404, "账号不存在")
	}
	if !truthy(acc["cookies"]) {
		return nil, httpError(400, "该账号没有 cookie")
	}
	cookies := toStringMap(acc["cookies"])
	expires := cookieIntMap(acc["cookies_exp"])

	var probe any
	_ = a.browser.runBlocking(c.R.Context().Done(), func() {
		probe = a.probeSession(aid, cookies, expires)
	})
	if m, ok := probe.(pyval.Obj); ok {
		return m, nil
	}
	if err, isErr := probe.(error); isErr {
		return nil, err
	}
	return pyval.Obj{{"ok", false}, {"message", "未执行"}}, nil
}

func (a *App) probeSession(aid string, cookies map[string]string, expires map[string]int) any {
	if err := a.Engine.Start(); err != nil {
		return a.testFailure(aid, err)
	}
	if _, err := a.Engine.Refresh(cookies, expires); err != nil {
		return a.testFailure(aid, err)
	}
	synced := a.syncCookies(aid)
	quota := any(nil)
	if q, err := a.Engine.Quota(cookies, expires); err == nil {
		q["checked_at"] = a.now().Unix()
		a.Store.UpdateAccount(aid, map[string]any{"quota": q})
		quota = q
	}
	a.Store.Mark(aid, true, "会话有效")
	return pyval.Obj{{"ok", true}, {"message", "会话有效，可正常生成"},
		{"synced", synced}, {"quota", quota}}
}

func (a *App) testFailure(aid string, err error) pyval.Obj {
	var ae *engine.ErrAuth
	if errors.As(err, &ae) {
		a.Store.Mark(aid, false, truncate(ae.Error(), 200))
		return pyval.Obj{{"ok", false}, {"message", truncate(ae.Error(), 200)}}
	}
	a.Store.TouchKeepalive(aid, nil, "测试未确认（保留账号状态）: "+truncate(err.Error(), 200))
	return pyval.Obj{{"ok", false}, {"message", truncate(err.Error(), 200)}}
}

// queryQuota 对应 query_quota()：打开 Settings 面板读实时额度并缓存到账号记录。
func (a *App) queryQuota(c *reqCtx) (any, error) {
	aid := c.Args["aid"]
	acc := a.Store.GetAccount(aid)
	if acc == nil {
		return nil, httpError(404, "账号不存在")
	}
	if !truthy(acc["cookies"]) {
		return nil, httpError(400, "该账号没有 cookie")
	}
	cookies := toStringMap(acc["cookies"])
	expires := cookieIntMap(acc["cookies_exp"])

	var (
		q    map[string]any
		pErr error
	)
	if err := a.browser.runBlocking(c.R.Context().Done(), func() {
		if serr := a.Engine.Start(); serr != nil {
			pErr = serr
			return
		}
		var qerr error
		q, qerr = a.Engine.Quota(cookies, expires)
		if qerr != nil {
			pErr = qerr
			return
		}
		q["checked_at"] = a.now().Unix()
		a.Store.UpdateAccount(aid, map[string]any{"quota": q})
	}); err != nil {
		return nil, err
	}
	if pErr != nil {
		var ae *engine.ErrAuth
		var ge *engine.ErrGeneration
		switch {
		case errors.As(pErr, &ae):
			a.Store.Mark(aid, false, truncate(ae.Error(), 200))
			return nil, httpError(401, ae.Error())
		case errors.As(pErr, &ge):
			return nil, httpError(502, ge.Error())
		}
		return nil, httpError(502, "额度查询失败: "+pErr.Error())
	}
	return q, nil
}

func (a *App) queryAnyQuota(c *reqCtx) (any, error) {
	acc := a.Store.PickAccount("", true, false, "")
	if acc == nil {
		return nil, httpError(400, "没有可用账号")
	}
	c.Args = map[string]string{"aid": strOf(acc["id"])}
	return a.queryQuota(c)
}

// reloginAll 对应 relogin()。
//
// DEVIATION: Python 这一条**不持 GEN_LOCK**（是 6 个取锁点之外的路径），Go 改成按
// test_account 那样 `with GEN_LOCK` 排队，理由有两条：
//
//	① 内存安全 —— Refresh 会换掉 e.page/e.browser，而并发的生成任务正在无锁读它们，
//	   Python 有 GIL 兜着只是「逻辑上互相踩」，Go 里这是数据竞争，最坏读到撕裂的接口值直接 panic；
//	② 逻辑上 Python 这条本来就没有正确结果 —— 任务跑到一半被 refresh 关掉标签页，
//	   两边都拿不到想要的东西。排队之后行为可预期，空闲时的对外结果与 Python 完全一致。
func (a *App) reloginAll(c *reqCtx) (any, error) {
	acc := a.Store.PickAccount("", true, false, "")
	if acc == nil {
		return nil, httpError(400, "没有可用账号")
	}
	cookies := toStringMap(acc["cookies"])
	expires := cookieIntMap(acc["cookies_exp"])
	var failure error
	if err := a.browser.runBlocking(c.R.Context().Done(), func() {
		if err := a.Engine.Start(); err != nil {
			failure = err
			return
		}
		if _, err := a.Engine.Refresh(cookies, expires); err != nil {
			failure = err
		}
	}); err != nil {
		return reloginErr(err), nil
	}
	if failure != nil {
		return reloginErr(failure), nil
	}
	return pyval.Obj{{"ok", true}, {"account", acc["id"]}}, nil
}

func reloginErr(err error) pyval.Obj {
	return pyval.Obj{{"ok", false}, {"error", err.Error()}}
}
