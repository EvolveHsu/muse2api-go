// Package store 是账号池与任务存储（JSON 落盘，原子写），逐函数对应 Python 的 store.py。
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EssentialCookies 决定账号生死的核心 cookie（与 engine 侧保持一致）。
var EssentialCookies = []string{"hatch_sess", "hatch_gw", "hatch_vml", "hatch_native_auth_device"}

// VML_TTL：hatch_vml 固定约 2 天且不因使用而延长，它就是决定账号寿命的那条 cookie。
// 拿不到它的 expires 时按「导入时间 + 2 天」估算，而不是退回 hatch_native_auth_device
// 的 30 天（那个不决定生死，会误导用户）。
const VMLTTL = 2 * 86400

// accountKeyOrder / taskKeyOrder 是 Python dict 的插入顺序，落盘时按它输出，
// 让 Go 版写出的 JSON 与 Python 版键序一致、diff 可读。未知键排在末尾（按字母序）。
var accountKeyOrder = []string{
	"id", "label", "cookies", "cookies_exp", "expires_at", "expiry_anchor",
	"enabled", "created_at", "last_used", "last_keepalive", "use_count",
	"ok", "note", "synced_at", "checked_at", "quota",
}

var taskKeyOrder = []string{
	"id", "object", "kind", "prompt", "status", "created_at", "updated_at",
	"result", "error",
}

type kv struct {
	K string
	V any
}

// orderedObject 让 encoding/json 按给定键序输出。
type orderedObject struct{ items []kv }

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, it := range o.items {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(it.K)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := marshal(it.V)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func orderKeys(known []string, m map[string]any) []string {
	out := make([]string, 0, len(m))
	seen := map[string]bool{}
	for _, k := range known {
		if _, ok := m[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func orderAccount(a map[string]any) any {
	if a == nil {
		return map[string]any{}
	}
	items := make([]kv, 0, len(a))
	for _, k := range orderKeys(accountKeyOrder, a) {
		items = append(items, kv{k, a[k]})
	}
	return orderedObject{items}
}

func orderTask(t map[string]any) any {
	if t == nil {
		return map[string]any{}
	}
	items := make([]kv, 0, len(t))
	for _, k := range orderKeys(taskKeyOrder, t) {
		items = append(items, kv{k, t[k]})
	}
	return orderedObject{items}
}

// marshal 关掉了 Go 默认的 HTML 转义（Python 的 json.dump 不转义 <>&），
// 且非 ASCII 原样写出，等价于 ensure_ascii=False。
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func dumpIndent(v any) ([]byte, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", " "); err != nil { // Python indent=1
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------- 值语义辅助

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}

// nonzeroNum 对应 Python 的「a.get('expires_at') and ...」：只要是非零数值就成立（可为负）。
func nonzeroNum(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	n, ok := toInt(v)
	if !ok || n == 0 {
		return 0, false
	}
	return n, true
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true // Python int(12.7) == 12
	case int:
		return t, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// isPos 对应 _is_pos()：能转成 int 且 > 0。
func isPos(v any) bool {
	n, ok := toInt(v)
	return ok && n > 0
}

func posInt(v any) int {
	n, _ := toInt(v)
	return n
}

// uuidHex / nowUnix / nowFloat 是可变接缝，只为让落盘格式能与真 Python 的输出逐字节对比；
// 生产路径不会被改写。
var (
	uuidHex = func(n int) string {
		b := make([]byte, (n+1)/2)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return hex.EncodeToString(b)[:n]
	}
	nowUnix = func() int { return int(time.Now().Unix()) }
	// Python 的 time.time() 是浮点秒，last_used 就是用它写的。
	nowFloat = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

// MinExpiry 返回核心 cookie 里最早过期的那个；核心 cookie 都拿不到时对所有值兜底。
// 返回 int 或 nil（Python 的 int|None），必须保持裸值 —— 存成指针会让 isPos/统计静默失效。
func MinExpiry(cookiesExp map[string]any) any {
	if len(cookiesExp) == 0 {
		return nil
	}
	var vals []int
	for _, name := range EssentialCookies {
		if v, ok := cookiesExp[name]; ok && isPos(v) {
			vals = append(vals, posInt(v))
		}
	}
	if len(vals) == 0 {
		keys := make([]string, 0, len(cookiesExp))
		for k := range cookiesExp {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := cookiesExp[k]; isPos(v) {
				vals = append(vals, posInt(v))
			}
		}
	}
	if len(vals) == 0 {
		return nil
	}
	min := vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
	}
	return min
}

// AccountExpiry 是账号实际失效时间：hatch_vml 真值 > baseTS+VML_TTL 估算 > 核心 cookie 最早过期。
// baseTS 为 0 表示 Python 里的 None。返回 int 或 nil。
func AccountExpiry(cookiesExp map[string]any, baseTS int) any {
	var vml any
	if cookiesExp != nil {
		vml = cookiesExp["hatch_vml"]
	}
	if isPos(vml) {
		return posInt(vml)
	}
	if baseTS != 0 {
		return baseTS + VMLTTL
	}
	return MinExpiry(cookiesExp)
}

func essentialOK(cookies map[string]any) bool {
	for _, n := range EssentialCookies {
		if !truthy(cookies[n]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- Store

type Config interface {
	AccountsFile() string
	TasksFile() string
}

type Store struct {
	cfg Config

	mu       sync.Mutex // 对应模块级 _LOCK
	accounts []map[string]any
	tasks    map[string]map[string]any

	stickyID    string
	stickyCount int
}

func readJSON(path string) any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// write 对应 _write()：同目录临时文件 + 原子 rename。
func write(path string, obj any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := dumpIndent(obj)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".muse2api-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func asAccountList(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func asTaskMap(v any) map[string]map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]map[string]any{}
	}
	out := make(map[string]map[string]any, len(m))
	for k, it := range m {
		if t, ok := it.(map[string]any); ok {
			out[k] = t
		}
	}
	return out
}

// New 对应 Store.__init__：读盘后把有 hatch_vml 真值的账号 expires_at 归一。
func New(cfg Config) *Store {
	s := &Store{cfg: cfg, tasks: map[string]map[string]any{}}
	s.accounts = asAccountList(readJSON(cfg.AccountsFile()))
	for _, a := range s.accounts {
		ce, _ := a["cookies_exp"].(map[string]any)
		if isPos(ce["hatch_vml"]) {
			a["expires_at"] = posInt(ce["hatch_vml"])
		}
	}
	s.tasks = asTaskMap(readJSON(cfg.TasksFile()))
	return s
}

func (s *Store) persistAccounts() error {
	return write(s.cfg.AccountsFile(), s.accountsView())
}

func (s *Store) accountsView() []any {
	out := make([]any, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, orderAccount(a))
	}
	return out
}

func (s *Store) find(aid string) map[string]any {
	for _, a := range s.accounts {
		if a["id"] == aid {
			return a
		}
	}
	return nil
}

// AddAccount 对应 add_account()。
func (s *Store) AddAccount(cookies map[string]any, label string, cookiesExp map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	aid := uuidHex(12)
	exp := map[string]any{}
	for k, v := range cookiesExp {
		if isPos(v) {
			exp[k] = posInt(v)
		}
	}
	now := nowUnix()
	acc := map[string]any{
		"id":          aid,
		"label":       firstNonEmpty(label, aid),
		"cookies":     cookies,
		"cookies_exp": exp,
		"expires_at":  AccountExpiry(exp, now),
		// expiry_anchor 的初始值是 None；落盘时按 Python 那样保留显式 null。
		"expiry_anchor":  nil,
		"enabled":        true,
		"created_at":     now,
		"last_used":      nil,
		"last_keepalive": nil,
		"use_count":      0,
		"ok":             nil,
		"note":           "",
		"synced_at":      nil,
	}
	s.accounts = append(s.accounts, acc)
	_ = s.persistAccounts()
	return acc
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ListAccounts 对应 list_accounts()：去掉 cookies，补上 cookie_count / essential_ok / expires_estimated。
func (s *Store) ListAccounts() []map[string]any {
	out := make([]map[string]any, 0, len(s.accounts))
	for _, a := range s.accounts {
		item := map[string]any{}
		for k, v := range a {
			if k == "cookies" {
				continue
			}
			item[k] = v
		}
		ce, _ := a["cookies_exp"].(map[string]any)
		cookies, _ := a["cookies"].(map[string]any)
		if isPos(ce["hatch_vml"]) {
			item["expires_at"] = posInt(ce["hatch_vml"])
		}
		item["cookie_count"] = len(cookies)
		item["essential_ok"] = essentialOK(cookies)
		item["expires_estimated"] = !isPos(ce["hatch_vml"])
		out = append(out, item)
	}
	return out
}

// DeleteAccount 对应 delete_account()。
func (s *Store) DeleteAccount(aid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.accounts)
	kept := make([]map[string]any, 0, n)
	for _, a := range s.accounts {
		if a["id"] != aid {
			kept = append(kept, a)
		}
	}
	s.accounts = kept
	_ = s.persistAccounts()
	return len(s.accounts) != n
}

// PickAccount 选择可用账号：优先复用已预热的健康账号（减少切号重连耗时），每满 15 次或遇错自动轮转。
// PickAccount 对应上游 1.6.0 的 store.pick_account 新签名：
// excludeIDs 是「正在被其它并发任务占用」的账号集合（nil = 上游语义里的 None，
// 空但非 nil 的切片 = 上游语义里的空 set，两者在「全都繁忙」时的返回值不同）；
// recordUse=false 时只做选择，不更新 last_used/use_count，也不落盘。
func (s *Store) PickAccount(preferredID string, rotate, forceRotate bool, excludeID string,
	excludeIDs []string, recordUse bool) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	var live []map[string]any
	for _, a := range s.accounts {
		if enabledIs(a, true) && truthy(a["cookies"]) {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return nil
	}
	blocked := map[string]bool{}
	for _, id := range excludeIDs {
		blocked[id] = true
	}
	if excludeID != "" {
		blocked[excludeID] = true
	}
	if len(blocked) > 0 {
		var available []map[string]any
		for _, a := range live {
			if !blocked[strOfID(a)] {
				available = append(available, a)
			}
		}
		if len(available) > 0 {
			live = available
		} else if excludeIDs != nil {
			// 并发模式下要求避开所有繁忙账号，但当前无剩余空闲账号。
			return nil
		} else if len(live) > 1 {
			var filtered []map[string]any
			for _, a := range live {
				if strOfID(a) != excludeID {
					filtered = append(filtered, a)
				}
			}
			if len(filtered) > 0 {
				live = filtered
			}
		}
	}

	if preferredID != "" && !forceRotate {
		for _, a := range live {
			if a["id"] == preferredID && !isFalse(a["ok"]) {
				stickyN := 0
				if s.stickyID == preferredID {
					stickyN = s.stickyCount
				}
				if !rotate || stickyN < 15 {
					s.stickyID = preferredID
					s.stickyCount = stickyN + 1
					if recordUse {
						a["last_used"] = nowFloat()
						a["use_count"] = posInt(a["use_count"]) + 1
						_ = s.persistAccounts()
					}
					return a
				}
			}
		}
	}

	var healthy []map[string]any
	for _, a := range live {
		if !isFalse(a["ok"]) {
			healthy = append(healthy, a)
		}
	}
	candidates := healthy
	if len(candidates) == 0 {
		candidates = live
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		lu := numOr0(ai["last_used"])
		lv := numOr0(aj["last_used"])
		if lu != lv {
			return lu < lv
		}
		return numOr0(ai["use_count"]) < numOr0(aj["use_count"])
	})
	acc := candidates[0]
	s.stickyID, _ = acc["id"].(string)
	s.stickyCount = 1
	if recordUse {
		acc["last_used"] = nowFloat()
		acc["use_count"] = posInt(acc["use_count"]) + 1
		_ = s.persistAccounts()
	}
	return acc
}

func strOfID(a map[string]any) string {
	v, _ := a["id"].(string)
	return v
}

// isFalse 对应 Python 的 `x is False`（None 不算 False）。
func isFalse(v any) bool {
	b, ok := v.(bool)
	return ok && !b
}

// enabledIs 对应 a.get("enabled", True)。
func enabledIs(a map[string]any, def bool) bool {
	v, ok := a["enabled"]
	if !ok || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return truthy(v)
}

func numOr0(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case nil:
		return 0
	}
	if n, ok := toInt(v); ok {
		return float64(n)
	}
	return 0
}

// TouchKeepalive 对应 touch_keepalive()：ok=nil 时仅记录未确认检测，保留上次状态、保活时间与有效期。
func (s *Store) TouchKeepalive(aid string, ok *bool, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowTS := nowUnix()
	for _, a := range s.accounts {
		if a["id"] != aid {
			continue
		}
		if ok != nil {
			a["ok"] = *ok
		}
		if ok != nil && *ok == true {
			a["last_keepalive"] = nowTS
			// 只信实际 cookie 到期时间或原导入锚点，不凭检测成功虚推 48h。
			ce, _ := a["cookies_exp"].(map[string]any)
			a["expires_at"] = AccountExpiry(ce, expiryAnchor(a))
		}
		if note != "" {
			a["note"] = clip(note, 300)
		}
		a["checked_at"] = nowTS
	}
	_ = s.persistAccounts()
}

// expiryAnchor 对应 Python 的 `a.get("expiry_anchor") or a.get("created_at")`；0 表示 None。
func expiryAnchor(a map[string]any) int {
	if v, ok := a["expiry_anchor"]; ok && isPos(v) {
		return posInt(v)
	}
	if c, ok := a["created_at"]; ok && isPos(c) {
		return posInt(c)
	}
	return 0
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Mark 对应 mark()。
func (s *Store) Mark(aid string, ok bool, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a["id"] != aid {
			continue
		}
		a["ok"] = ok
		a["note"] = clip(note, 300)
		a["checked_at"] = nowUnix()
	}
	_ = s.persistAccounts()
}

// GetAccount 对应 get_account()（返回内部对象，与 Python 一样是同一份引用）。
func (s *Store) GetAccount(aid string) map[string]any {
	return s.find(aid)
}

// UpdateAccount 对应 update_account(**kw)：nil 值跳过；传了 cookies_exp 自动重算 expires_at。
//
// expiry_anchor 维护规则：读到 hatch_vml 真值 -> 清空锚点；读不到且锚点为空 -> 钉在当下（只钉一次）；
// 重登补入全新会话 cookie 时由调用方显式传 expiry_anchor=now 重置。
func (s *Store) UpdateAccount(aid string, kw map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.find(aid)
	if a == nil {
		return nil
	}
	keys := make([]string, 0, len(kw))
	for k := range kw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := kw[k]
		if v == nil {
			continue
		}
		a[k] = v
	}
	if v, ok := kw["cookies_exp"]; ok && v != nil {
		ce, _ := a["cookies_exp"].(map[string]any)
		if isPos(ce["hatch_vml"]) {
			a["expiry_anchor"] = nil
		} else if !isPos(a["expiry_anchor"]) {
			a["expiry_anchor"] = nowUnix()
		}
		a["expires_at"] = AccountExpiry(ce, expiryAnchor(a))
	}
	_ = s.persistAccounts()
	return a
}

// Stats 对应 stats()。
func (s *Store) Stats() map[string]any {
	now := nowUnix()
	total := len(s.accounts)
	enabled, bad, healthy, expiring, expired := 0, 0, 0, 0, 0
	for _, a := range s.accounts {
		if !enabledIs(a, true) {
			continue
		}
		enabled++
		if isFalse(a["ok"]) {
			bad++
		}
		if b, ok := a["ok"].(bool); ok && b {
			healthy++
		}
		if n, ok := nonzeroNum(a["expires_at"]); ok {
			if n-now < 3*86400 {
				expiring++
			}
			if n <= now {
				expired++
			}
		}
	}
	return map[string]any{
		"total": total, "enabled": enabled, "disabled": total - enabled,
		"healthy": healthy, "error": bad, "expiring": expiring, "expired": expired,
	}
}

// ---------------------------------------------------------------- 任务

// CreateTask 对应 create_task()。
func (s *Store) CreateTask(kind, prompt string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	tid := "task_" + uuidHex(20)
	now := nowUnix()
	t := map[string]any{
		"id": tid, "object": "task", "kind": kind, "prompt": prompt,
		"status": "queued", "created_at": now, "updated_at": now,
		"result": nil, "error": nil,
	}
	s.tasks[tid] = t
	_ = s.persistTasks()
	return t
}

func (s *Store) persistTasks() error {
	view := make([]kv, 0, len(s.tasks))
	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	// Python dict 是插入顺序；这里按 created_at 升序输出，保证同一份数据两边写入结果稳定。
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.tasks[ids[i]], s.tasks[ids[j]]
		return numOr0(a["created_at"]) < numOr0(b["created_at"])
	})
	for _, id := range ids {
		view = append(view, kv{id, orderTask(s.tasks[id])})
	}
	return write(s.cfg.TasksFile(), orderedObject{view})
}

// UpdateTask 对应 update_task(**kw)。
func (s *Store) UpdateTask(tid string, kw map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[tid]
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(kw))
	for k := range kw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t[k] = kw[k]
	}
	t["updated_at"] = nowUnix()
	_ = s.persistTasks()
	return t
}

// FailInterruptedTasks 对应 app.py 的 @app.on_event("startup") 里那段循环：
// 进程重启时还挂在 queued/processing 的任务永远等不到结果（工作线程是内存态的），
// 当场判失败落盘，客户端才会看到「请重新提交」而不是一直 pending。
// 上游 1.6.0 起视频任务也纳入清理（此前只清 image）。
func (s *Store) FailInterruptedTasks(message string) int {
	var ids []string
	s.mu.Lock()
	for tid, t := range s.tasks {
		if t["kind"] != "image" && t["kind"] != "video" {
			continue
		}
		switch t["status"] {
		case "queued", "processing":
			ids = append(ids, tid)
		}
	}
	s.mu.Unlock()
	for _, tid := range ids {
		s.UpdateTask(tid, map[string]any{"status": "failed", "error": message})
	}
	return len(ids)
}

// GetTask 对应 get_task()。
func (s *Store) GetTask(tid string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[tid]
}

// ListTasks 对应 list_tasks(limit)：按 created_at 倒序。
func (s *Store) ListTasks(limit int) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]map[string]any, 0, len(s.tasks))
	for _, t := range s.tasks {
		items = append(items, t)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return numOr0(items[i]["created_at"]) > numOr0(items[j]["created_at"])
	})
	if limit >= 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// DeleteTask 对应 delete_task()。
func (s *Store) DeleteTask(tid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[tid]; !ok {
		return false
	}
	delete(s.tasks, tid)
	_ = s.persistTasks()
	return true
}

// ClearTasks 对应 clear_tasks(keep)，返回删除条数。
func (s *Store) ClearTasks(keep int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.listTasksLocked(-1)
	keepIDs := map[string]bool{}
	for i, t := range items {
		if i >= keep {
			break
		}
		if id, ok := t["id"].(string); ok {
			keepIDs[id] = true
		}
	}
	removed := len(s.tasks) - len(keepIDs)
	for id := range s.tasks {
		if !keepIDs[id] {
			delete(s.tasks, id)
		}
	}
	_ = s.persistTasks()
	if removed < 0 {
		return 0
	}
	return removed
}

func (s *Store) listTasksLocked(limit int) []map[string]any {
	items := make([]map[string]any, 0, len(s.tasks))
	for _, t := range s.tasks {
		items = append(items, t)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return numOr0(items[i]["created_at"]) > numOr0(items[j]["created_at"])
	})
	if limit >= 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// Accounts 暴露内部切片仅供测试与迁移使用。
func (s *Store) Accounts() []map[string]any { return s.accounts }

// HasLiveAccounts 对应上游 AccountPoolManager.acquire() 里那句
// `live = [a for a in self.store.accounts if a.get("enabled", True) and a.get("cookies")]`
// 的判空：一个可路由的账号都没有时要直接报「没有可用账号」，而不是等超时。
func (s *Store) HasLiveAccounts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if enabledIs(a, true) && truthy(a["cookies"]) {
			return true
		}
	}
	return false
}
