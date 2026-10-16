package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"muse2api-go/internal/engine"
	"muse2api-go/internal/pyval"
)

// browserLock 对应 GEN_LOCK。上游 1.6.0 把它从「全局一把串行锁」升级成
// ConcurrencyLock（信号量，槽位数 MUSE2API_CONCURRENCY，默认 2，下限 1）。
// Python 用的是三种取法：带 timeout 的、阻塞的、blocking=False 的；
// Go 用容量 N 的 channel 实现，三种语义在这里分开给名字，避免调用点混用。
type browserLock struct {
	ch chan struct{}
	// DEVIATION: Python 的 GEN_LOCK 就是一把裸锁，没有 last 这个字段。
	// 它只为空闲回收（lifecycle.go 的 idleEvictor）服务：记录最近一次用过浏览器的时刻。
	// 存的是带单调读数的 time.Time，所以 idleFor 不受系统校时拉动。
	mu   sync.Mutex
	last time.Time
}

func newBrowserLock(limit int) *browserLock {
	if limit < 1 {
		limit = 1
	}
	return &browserLock{ch: make(chan struct{}, limit), last: time.Now()}
}

// locked 对应 ConcurrencyLock.locked()：有任意槽位在用即为真。
func (b *browserLock) locked() bool { return len(b.ch) > 0 }

// touch 记一次「用过浏览器」。拿到和放开的时候都记，
// 于是一段长任务跑完之后的静默期才是从「跑完」起算，而不是从「开始」起算。
func (b *browserLock) touch() {
	b.mu.Lock()
	b.last = time.Now()
	b.mu.Unlock()
}

// busy 看锁现在有没有人被占着（容量 1 的 channel，len>0 就是被占）。
func (b *browserLock) busy() bool { return len(b.ch) > 0 }

// idleFor 是距离上次用浏览器过去了多久。
func (b *browserLock) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.last)
}

// run 对应 `acquire(timeout=...)`：等不到就报「等待浏览器队列超时，请稍后重试」。
func (b *browserLock) run(timeout time.Duration, fn func() error) error {
	if timeout < time.Second {
		timeout = time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case b.ch <- struct{}{}:
	case <-timer.C:
		return &engine.ErrGeneration{Msg: "等待浏览器队列超时，请稍后重试"}
	}
	b.touch()
	defer func() { <-b.ch; b.touch() }()
	return fn()
}

// runBlocking 对应 `with GEN_LOCK:` —— 无限等。
func (b *browserLock) runBlocking(stop <-chan struct{}, fn func()) error {
	if stop == nil {
		b.ch <- struct{}{}
		b.touch()
		defer func() { <-b.ch; b.touch() }()
		fn()
		return nil
	}
	select {
	case b.ch <- struct{}{}:
	case <-stop:
		return errStopped
	}
	b.touch()
	defer func() { <-b.ch; b.touch() }()
	fn()
	return nil
}

// tryRun 对应 `acquire(blocking=False)`：抢不到就什么都不做（额度探测、预热用）。
func (b *browserLock) tryRun(fn func()) bool {
	select {
	case b.ch <- struct{}{}:
		b.touch()
		defer func() { <-b.ch; b.touch() }()
		fn()
		return true
	default:
		return false
	}
}

var errStopped = errors.New("客户端已断开连接，终止生成任务")

// renewAndPersist 对应 _renew_and_persist()：
// 10 分钟内已续签过且状态正常的账号直接复用，免得每次请求都阻塞 2~3 秒 HTTP 往返。
func (a *App) renewAndPersist(accID string, wakeVM, force bool) (map[string]any, error) {
	acc := a.Store.GetAccount(accID)
	if acc == nil || !truthy(acc["cookies"]) {
		return acc, nil
	}
	now := float64(a.now().Unix())
	lastSync := maxF(numOr(acc["synced_at"]), numOr(acc["last_keepalive"]),
		a.Engine.LastHTTPRenew(accID))
	if !force && acc["ok"] == true && now-lastSync < 600 {
		a.Engine.SetLastHTTPRenew(accID, lastSync)
		return acc, nil
	}
	cookies := cookieMap(acc["cookies"])
	res, err := a.Engine.RenewSessionHTTP(cookies, cookieIntMap(acc["cookies_exp"]), wakeVM)
	if err != nil {
		a.Store.Mark(accID, false, err.Error())
		return nil, err
	}
	a.Engine.SetLastHTTPRenew(accID, now)
	if res != nil && len(res.Cookies) > 0 {
		okVal := acc["ok"]
		if res.OK {
			okVal = true
		}
		a.Store.UpdateAccount(accID, map[string]any{
			"cookies": toStringAny(res.Cookies), "cookies_exp": toIntAny(res.CookiesExp),
			"ok": okVal, "synced_at": int64(now)})
		vm := "RUNNING"
		if res.VMState != "" {
			vm = res.VMState
		}
		a.Store.TouchKeepalive(accID, boolPtrT(res.OK),
			fmt.Sprintf("会话正常 (VM: %s)", vm))
	}
	return a.Store.GetAccount(accID), nil
}

// syncCookies 对应 _sync_cookies()。
// 注意：生成已经成功了，这里纯粹锦上添花，出任何问题都只记一条日志，绝不能把生成搞失败。
// syncCookies 的返回值是要原样吐给客户端的，所以用有序对象：Python 那边是 dict 字面量，
// 键序 synced → changed → expires_at（用 map 会被 encoding/json 按字母排）。
func (a *App) syncCookies(ctx *engine.PageCtx, accID string) pyval.Obj {
	live := a.Engine.ReadCookies(ctx)
	if len(live) == 0 {
		return pyval.Obj{{"synced", 0}}
	}
	acc := a.Store.GetAccount(accID)
	if acc == nil {
		return pyval.Obj{{"synced", 0}}
	}
	cur := toStringMap(acc["cookies"])
	newVals := map[string]string{}
	for k, v := range live {
		if v.Value != "" {
			newVals[k] = v.Value
		}
	}
	// 有效期要「合并」而不是「覆盖」：会话 cookie 读回的 expires 是 -1，
	// 若整体覆盖会把原有的有效期记录清空（实测踩过）。
	exps := toIntMap(acc["cookies_exp"])
	for k, v := range live {
		if pos(v.Expires) {
			exps[k] = int(v.Expires)
		}
	}
	changed := 0
	for k, v := range newVals {
		if cur[k] != v {
			changed++
		}
	}
	merged := map[string]string{}
	for k, v := range cur {
		merged[k] = v
	}
	for k, v := range newVals {
		merged[k] = v
	}
	a.Store.UpdateAccount(accID, map[string]any{
		"cookies": toStringAny(merged), "cookies_exp": exps, "synced_at": int64(a.now().Unix())})
	updated := a.Store.GetAccount(accID)
	// 键序照 Python 的 dict 字面量：synced → changed → expires_at。
	out := pyval.Obj{{"synced", len(newVals)}, {"changed", changed}}
	if updated != nil {
		out = append(out, pyval.Pair{"expires_at", updated["expires_at"]})
	}
	return out
}

// runGeneration 对应 _run_generation()：浏览器槽位所有权覆盖选号、重试与清理，不只是 generate。
// onAcquired 对应上游 1.6.1 新增的 on_acquired 回调：拿到槽位后先回调（给任务打 processing
// 并重置计时起点），排队等待的时间不计入任务耗时。
func (a *App) runGeneration(prompt, kind string, timeout int, accountID string,
	onProgress func(int), referenceImage string, stop <-chan struct{},
	onAcquired func()) (*engine.GenResult, string, error) {

	wait := time.Duration(max1(timeout)) * time.Second
	deadline := a.mono().Add(wait)
	var res *engine.GenResult
	var accID string
	err := a.browser.run(wait, func() error {
		if onAcquired != nil {
			// Python：`try: on_acquired() except Exception: pass`。
			func() {
				defer func() { _ = recover() }()
				onAcquired()
			}()
		}
		var e error
		res, accID, e = a.runGenerationLocked(prompt, kind, timeout, accountID,
			onProgress, referenceImage, deadline, stop)
		return e
	})
	if err != nil {
		return nil, "", err
	}
	return res, accID, nil
}

// preferVideoAccount 对应上游 1.6.1：视频任务优先复用「上一次真正完成过视频」的账号 ——
// 光看会话健康度不够，能出片才是硬标准。
func (a *App) preferVideoAccount() string {
	recent := a.Store.ListTasks(-1)
	sort.SliceStable(recent, func(i, j int) bool {
		return numOr(recent[i]["created_at"]) > numOr(recent[j]["created_at"])
	})
	for _, task := range recent {
		if task["kind"] != "video" || task["status"] != "completed" || !truthy(task["account"]) {
			continue
		}
		candidate := a.Store.GetAccount(strOf(task["account"]))
		if candidate != nil && accountEnabled(candidate) && !okIsFalse(candidate["ok"]) {
			return strOf(candidate["id"])
		}
	}
	return ""
}

func (a *App) runGenerationLocked(prompt, kind string, timeout int, accountID string,
	onProgress func(int), referenceImage string, deadline time.Time,
	stop <-chan struct{}) (*engine.GenResult, string, error) {

	remainingWait := int(deadline.Sub(a.mono()).Seconds())
	preferredID := a.Engine.CurrentAccountID()
	if kind == "video" && accountID == "" {
		if vidID := a.preferVideoAccount(); vidID != "" {
			preferredID = vidID
		}
	}
	if remainingWait < 1 {
		remainingWait = 1
	}
	acc, err := a.accounts.acquire(accountID, preferredID, true, false,
		time.Duration(remainingWait)*time.Second, true)
	if err != nil {
		return nil, "", err
	}
	cur := acc
	ctx := a.Engine.NewPageCtx()
	var lastErr error
	defer func() {
		if cur != nil {
			a.accounts.release(strOf(cur["id"]))
		}
		a.Engine.CloseCurrentPage(ctx)
	}()

	for attempt := 0; attempt < 2; attempt++ {
		if !a.mono().Before(deadline) {
			return nil, "", &engine.ErrGeneration{Msg: "任务总等待时限已到，停止重试"}
		}
		if attempt > 0 {
			if lastErr != nil && strings.Contains(lastErr.Error(), "未产出媒体附件，仅返回了文本回复") {
				break
			}
			oldID := ""
			if cur != nil {
				oldID = strOf(cur["id"])
			}
			if oldID != "" {
				a.accounts.release(oldID)
			}
			cur = nil
			rem := int(deadline.Sub(a.mono()).Seconds())
			if rem <= 0 {
				break
			}
			alt, aerr := a.accounts.acquire("", "", true, true,
				time.Duration(rem)*time.Second, true)
			if aerr != nil {
				break
			}
			if alt == nil || (oldID != "" && strOf(alt["id"]) == oldID) {
				if alt != nil {
					cur = alt
				}
				break
			}
			cur = alt
			log.Printf("【生图/视频自动切号】切换到备用账号 %s (%s) 重试...",
				strOf(cur["label"]), strOf(cur["id"]))
		}
		res, accID, err := a.attemptGenerate(ctx, cur, prompt, kind, timeout, onProgress,
			referenceImage, deadline, attempt > 0, stop)
		if err == nil {
			return res, accID, nil
		}
		lastErr = err
		var ae *engine.ErrAuth
		var ge *engine.ErrGeneration
		switch {
		case errors.As(err, &ae):
			a.Store.Mark(strOf(cur["id"]), false, ae.Error())
			a.Engine.CloseCurrentPage(ctx)
		case errors.As(err, &ge):
			a.Store.Mark(strOf(cur["id"]), true,
				"任务异常: "+truncate(ge.Error(), 60))
			a.Engine.ResetThread(ctx, false)
		default:
			a.Engine.CloseCurrentPage(ctx)
			lastErr = &engine.ErrGeneration{Msg: "生成失败: " + err.Error()}
		}
	}
	if lastErr == nil {
		lastErr = &engine.ErrGeneration{Msg: "生成失败"}
	}
	return nil, "", lastErr
}

func (a *App) attemptGenerate(ctx *engine.PageCtx, acc map[string]any, prompt, kind string, timeout int,
	onProgress func(int), referenceImage string, deadline time.Time, force bool,
	stop <-chan struct{}) (*engine.GenResult, string, error) {

	accID := strOf(acc["id"])
	refreshed, err := a.renewAndPersist(accID, true, force)
	if err != nil {
		return nil, "", err
	}
	if refreshed != nil {
		acc = refreshed
		accID = strOf(acc["id"])
	}
	if err := a.Engine.Start(); err != nil {
		return nil, "", err
	}
	remaining := int(deadline.Sub(a.mono()).Seconds())
	if remaining <= 0 {
		return nil, "", &engine.ErrGeneration{Msg: "任务总等待时限已到，停止重试"}
	}
	res, err := a.Engine.Generate(ctx, toStringMap(acc["cookies"]), prompt, kind, remaining,
		cookieIntMap(acc["cookies_exp"]), accID, onProgress, referenceImage, stop)
	if err != nil {
		return nil, "", err
	}
	a.Store.Mark(accID, true, "")
	a.syncCookies(ctx, accID)
	return res, accID, nil
}

// imageResponse 对应 _image_response()。
func (a *App) imageResponse(req *ImageRequest, res *engine.GenResult) pyval.Obj {
	item := pyval.Obj{
		{"revised_prompt", req.Prompt},
		{"url", a.mediaURL(res.Filename)},
		{"size", orDefaultStr(deref(req.Size), "auto")},
		{"kind", res.Kind},
		{"bytes", res.Size},
	}
	if req.ResponseFormat == "b64_json" {
		p := res.Path
		if p == "" {
			p = filepath.Join(a.CFG.MediaDir(), res.Filename)
		}
		data, err := os.ReadFile(p)
		if err == nil {
			item = append(item, pairOf("b64_json", encodeB64(data)))
			item = dropKey(item, "url")
		}
	}
	return pyval.Obj{
		{"created", a.now().Unix()}, {"data", []any{item}},
	}
}

func dropKey(o pyval.Obj, key string) pyval.Obj {
	out := make(pyval.Obj, 0, len(o))
	for _, p := range o {
		if p.Key != key {
			out = append(out, p)
		}
	}
	return out
}

func pairOf(k string, v any) pyval.Pair { return pyval.Pair{Key: k, Val: v} }

// queueImage 对应 _queue_image()：可选的轮询模式，幂等键复用同一个任务，
// 以及「一个 Chromium 工人，靠限流而不是加队列中间件」。
func (a *App) queueImage(c *reqCtx, req *ImageRequest, prompt, referenceImage string,
	idempotencyKey *string) (any, error) {
	key := ""
	if idempotencyKey != nil {
		key = *idempotencyKey
	}
	if key != "" && len([]rune(key)) > 256 {
		return nil, httpError(400, "Idempotency-Key too long")
	}
	var keyHash *string
	if key != "" {
		h := sha256.Sum256([]byte(key))
		s := hex.EncodeToString(h[:])
		keyHash = &s
	}
	payload := pyval.Obj{
		{"request", dumpImage(req)},
		{"reference", nilIfEmpty(referenceImage)},
	}
	requestHash := sha256.Sum256([]byte(pyval.Dumps(payload, false, true)))
	reqHashStr := hex.EncodeToString(requestHash[:])

	a.imgTaskLock.Lock()
	tasks := a.Store.ListTasks(-1) // Python 是 list(store.tasks.values())，全量
	if keyHash != nil {
		for _, existing := range tasks {
			if strOf(existing["image_request_key"]) != *keyHash {
				continue
			}
			if strOf(existing["image_request_hash"]) != reqHashStr {
				a.imgTaskLock.Unlock()
				return nil, httpError(409, "Idempotency-Key reused with different request")
			}
			a.imgTaskLock.Unlock()
			return &rawResponse{Status: 202, Headers: map[string]string{
				"content-type": "application/json"},
				Body: []byte(pyval.MarshalCompact(pyval.Obj{
					{"id", existing["id"]}, {"task_id", existing["id"]},
					{"object", "image.task"}, {"status", existing["status"]},
					{"progress", orDefaultNum(existing["progress"], 0)},
					{"created_at", existing["created_at"]},
				}))}, nil
		}
	}
	queued := 0
	for _, t := range tasks {
		if t["kind"] == "image" && (strOf(t["status"]) == "queued" || strOf(t["status"]) == "processing") {
			queued++
		}
	}
	if queued >= 8 {
		a.imgTaskLock.Unlock()
		return nil, httpError(429, "Image queue is full; retry later")
	}
	task := a.Store.CreateTask("image", req.Prompt)
	tid := strOf(task["id"])
	a.Store.UpdateTask(tid, map[string]any{
		"api_prompt": prompt, "size": ptrVal(req.Size),
		"response_format": req.ResponseFormat, "progress": 0,
		"image_request_key": ptrVal(keyHash), "image_request_hash": reqHashStr})
	a.imgTaskLock.Unlock()

	createdAt := task["created_at"]
	go func() {
		t0 := a.now()
		onAcquired := func() {
			// 拿到浏览器槽位才开始计时/进 processing：排队等待不算任务耗时（上游 1.6.1）。
			t0 = a.now()
			a.Store.UpdateTask(tid, map[string]any{"status": "processing", "progress": 5})
		}
		res, accID, err := a.runGeneration(prompt, "image",
			int(pyNumOr(req.Timeout, int64(a.CFG.ImageTimeout))), "",
			func(p int) { a.Store.UpdateTask(tid, map[string]any{"progress": p}) },
			referenceImage, nil, onAcquired)
		if err != nil {
			a.Store.UpdateTask(tid, map[string]any{
				"status": "failed", "error": err.Error(),
				"elapsed": round1(a.now().Sub(t0).Seconds())})
			return
		}
		u := a.mediaURL(res.Filename)
		a.Store.UpdateTask(tid, map[string]any{
			"status": "completed", "progress": 100, "account": accID,
			"elapsed": round1(a.now().Sub(t0).Seconds()), "url": u,
			"result": pyval.Obj{
				{"filename", res.Filename}, {"size", res.Size},
				{"kind", res.Kind}, {"url", u},
			}})
	}()

	return &rawResponse{Status: 202, Headers: map[string]string{"content-type": "application/json"},
		Body: []byte(pyval.MarshalCompact(pyval.Obj{
			{"id", tid}, {"task_id", tid}, {"object", "image.task"},
			{"status", "queued"}, {"progress", 0}, {"created_at", createdAt},
		}))}, nil
}

// refFromImages 把 images[0] 里能当参考图的值取出来（ChatGPT 风格与 URL 两种形态都兼容）。
func refFromImages(images []any) string {
	if len(images) == 0 {
		return ""
	}
	first := images[0]
	if m, ok := first.(map[string]any); ok {
		if v := strOf(m["image_url"]); v != "" {
			return v
		}
		return strOf(m["url"])
	}
	if s, ok := first.(string); ok {
		return s
	}
	return ""
}

func refFromImage(image any) string {
	switch v := image.(type) {
	case string:
		return v
	case map[string]any:
		if u := strOf(v["url"]); u != "" {
			return u
		}
		return strOf(v["b64_json"])
	}
	return ""
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
