package store

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeCfg struct{ dir string }

func (f fakeCfg) AccountsFile() string { return filepath.Join(f.dir, "accounts.json") }
func (f fakeCfg) TasksFile() string    { return filepath.Join(f.dir, "tasks.json") }

func newStore(t *testing.T) (*Store, fakeCfg) {
	t.Helper()
	cfg := fakeCfg{dir: t.TempDir()}
	return New(cfg), cfg
}

// 账号寿命优先级：hatch_vml 真值 > 导入锚点+2天 > 核心 cookie 最早过期。
func TestAccountExpiryPriority(t *testing.T) {
	ce := map[string]any{"hatch_vml": 1700086400, "hatch_sess": 1700000000}
	if got := AccountExpiry(ce, 1699000000); got != 1700086400 {
		t.Fatalf("有 hatch_vml 真值时应取真值，得到 %#v", got)
	}

	// 没有 hatch_vml：按锚点 + 2 天，而不是退回 hatch_native_auth_device 的 30 天。
	ce2 := map[string]any{"hatch_native_auth_device": 1900000000}
	if got := AccountExpiry(ce2, 1699000000); got != 1699000000+VMLTTL {
		t.Fatalf("应按锚点+2天估算，得到 %#v", got)
	}
	if got := AccountExpiry(ce2, 0); got != 1900000000 {
		t.Fatalf("无锚点时应退回最早过期，得到 %#v", got)
	}
	if got := AccountExpiry(nil, 0); got != nil {
		t.Fatalf("空输入应为 nil，得到 %#v", got)
	}
	// 返回值必须是裸 int：存成指针会让 isPos 与统计静默失效。
	if _, ok := AccountExpiry(ce, 0).(int); !ok {
		t.Fatal("AccountExpiry 必须返回裸 int 而不是 *int")
	}
}

func TestIsPosCoercion(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"int", 12, true},
		{"float 截断", 12.7, true},
		{"数字串", "123", true},
		{"带空格的数字串", " 45 ", true},
		{"非数字串", "abc", false},
		{"nil", nil, false},
		{"零", 0, false},
		{"负数", -5, false},
		{"true", true, true}, // Python bool(True) == 1 -> >0
		{"false", false, false},
	}
	for _, c := range cases {
		if got := isPos(c.v); got != c.want {
			t.Errorf("%s: isPos(%#v)=%v 期望 %v", c.name, c.v, got, c.want)
		}
	}
}

func TestAddAccountInitialShape(t *testing.T) {
	s, _ := newStore(t)
	acc := s.AddAccount(map[string]any{"hatch_sess": "s1"}, " Shaw ", map[string]any{
		"hatch_vml": "1700086400", "bad": "abc", "zero": 0,
	})
	if acc["label"] != " Shaw " {
		t.Errorf("label 不该被 trim（照抄 Python）: %q", acc["label"])
	}
	exp := acc["cookies_exp"].(map[string]any)
	if _, ok := exp["bad"]; ok {
		t.Error("非正数的 expires 应被过滤掉")
	}
	if exp["hatch_vml"] != 1700086400 {
		t.Errorf("字符串数字应转成 int，得到 %#v", exp["hatch_vml"])
	}
	if e := acc["expires_at"]; e != 1700086400 {
		t.Errorf("expires_at 应为 hatch_vml 真值，得到 %#v", e)
	}
	if _, ok := acc["expiry_anchor"]; !ok {
		t.Error("expiry_anchor 必须显式存在且为 null（Python 会落盘这个键）")
	}
	if acc["ok"] != nil || acc["note"] != "" || acc["use_count"] != 0 {
		t.Errorf("初始字段不对: %v %v %v", acc["ok"], acc["note"], acc["use_count"])
	}
}

func TestLabelFallsBackToID(t *testing.T) {
	s, _ := newStore(t)
	acc := s.AddAccount(map[string]any{"hatch_sess": "s"}, "", nil)
	if acc["label"] != acc["id"] {
		t.Fatalf("label 为空时应等于 id，得到 %v", acc["label"])
	}
}

// 空 cookies 字典在 Python 里是 falsy，不能算可用账号。
func TestPickAccountSkipsEmptyCookies(t *testing.T) {
	s, _ := newStore(t)
	s.AddAccount(map[string]any{}, "no-cookies", nil)
	if got := s.PickAccount("", true, false, "", nil, true); got != nil {
		t.Fatalf("空 cookie 账号不应被选中: %v", got)
	}
}

// ok 是三态：None 和 True 都算健康，只有 False 才算坏。
func TestPickAccountTriStateHealth(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	b := s.AddAccount(map[string]any{"hatch_sess": "2"}, "B", nil)
	s.Mark(a["id"].(string), false, "坏了")

	got := s.PickAccount("", true, false, "", nil, true)
	if got["id"] != b["id"] {
		t.Fatalf("应跳过 ok=False 的账号，得到 %v", got["id"])
	}

	c := s.AddAccount(map[string]any{"hatch_sess": "3"}, "C", nil)
	s.Mark(b["id"].(string), false, "也坏了")
	s.Mark(c["id"].(string), false, "全坏了")
	// 全坏时 Python 退回 live 全集，不返回 None。
	if got := s.PickAccount("", true, false, "", nil, true); got == nil {
		t.Fatal("全坏时应有兜底候选")
	}
}

// 粘性复用：同一 preferred_id 最多连续 15 次，第 16 次才轮转。
func TestPickAccountStickyFifteen(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	b := s.AddAccount(map[string]any{"hatch_sess": "2"}, "B", nil)
	aID, bID := a["id"].(string), b["id"].(string)

	for i := 1; i <= 15; i++ {
		if got := s.PickAccount(aID, true, false, "", nil, true); got["id"] != aID {
			t.Fatalf("第 %d 次仍应粘在 A，得到 %v", i, got["id"])
		}
	}
	if got := s.PickAccount(aID, true, false, "", nil, true); got["id"] == aID {
		t.Fatal("第 16 次应轮转走")
	}
	if got := s.PickAccount(aID, true, true, "", nil, true); got["id"] != bID && got["id"] != aID {
		t.Fatalf("force_rotate 也要能选出账号，得到 %v", got["id"])
	}
}

// 1.6.0 的并发语义：exclude_ids 给过（非 nil）时，「全部被占用」必须返回 None 去等待；
// 只有旧的 exclude_id（exclude_ids=nil）才允许在全是同一个账号时兜底复用。
func TestPickAccountExcludeIDsSemantics(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	b := s.AddAccount(map[string]any{"hatch_sess": "2"}, "B", nil)
	aID, bID := a["id"].(string), b["id"].(string)

	if got := s.PickAccount("", true, false, "", []string{aID}, true); got == nil || got["id"] != bID {
		t.Fatalf("应避开 A 选 B，得到 %v", got)
	}
	if got := s.PickAccount("", true, false, "", []string{aID, bID}, true); got != nil {
		t.Fatalf("全繁忙时应返回 None，得到 %v", got["id"])
	}
	if got := s.PickAccount("", true, false, "", []string{}, true); got == nil {
		t.Fatal("空排除集应能选到账号")
	}

	// 旧调用（exclude_ids=nil + exclude_id）：只有一个账号时仍兜底返回它。
	s2, _ := newStore(t)
	only := s2.AddAccount(map[string]any{"hatch_sess": "9"}, "Only", nil)
	onlyID := only["id"].(string)
	if got := s2.PickAccount("", true, true, onlyID, nil, true); got == nil || got["id"] != onlyID {
		t.Fatalf("旧语义下应兜底给唯一账号，得到 %v", got)
	}
}

// record_use=false 只做选择，不更新 last_used/use_count（预热任务不改变轮换计数）。
func TestPickAccountRecordUseFalse(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	aID := a["id"].(string)
	before := s.GetAccount(aID)
	if got := s.PickAccount("", false, false, "", nil, false); got == nil || got["id"] != aID {
		t.Fatalf("应选到唯一账号，得到 %v", got)
	}
	after := s.GetAccount(aID)
	if after["use_count"] != before["use_count"] || after["last_used"] != before["last_used"] {
		t.Fatalf("record_use=false 不该改计数: before=%v/%v after=%v/%v",
			before["use_count"], before["last_used"], after["use_count"], after["last_used"])
	}
}

// touch_keepalive(ok=None) 只记诊断，绝不能覆盖上次已确认的账号状态。
func TestTouchKeepaliveNilKeepsState(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	aid := a["id"].(string)
	s.Mark(aid, true, "会话有效")

	s.TouchKeepalive(aid, nil, "保活未确认（保留账号状态）: HTTP 503")
	got := s.GetAccount(aid)
	if got["ok"] != true {
		t.Fatalf("ok=None 不应改状态，得到 %v", got["ok"])
	}
	if got["last_keepalive"] != nil {
		t.Fatalf("ok=None 不应更新 last_keepalive，得到 %v", got["last_keepalive"])
	}
	if !isPos(got["checked_at"]) {
		t.Error("checked_at 应被记录")
	}
	if got["note"] != "保活未确认（保留账号状态）: HTTP 503" {
		t.Fatalf("note 没写进去: %v", got["note"])
	}
}

func TestNoteClippedTo300(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	long := ""
	for i := 0; i < 400; i++ {
		long += "啊"
	}
	s.Mark(a["id"].(string), false, long)
	if n := len([]rune(s.GetAccount(a["id"].(string))["note"].(string))); n != 300 {
		t.Fatalf("note 应裁到 300 字符，得到 %d", n)
	}
}

// expiry_anchor 规则：有 hatch_vml 真值就清锚点；没有则只钉一次。
func TestUpdateAccountAnchorRules(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	aid := a["id"].(string)

	s.UpdateAccount(aid, map[string]any{"cookies_exp": map[string]any{"hatch_sess": 1700000000}})
	first := s.GetAccount(aid)["expiry_anchor"]
	if !isPos(first) {
		t.Fatalf("读不到 hatch_vml 时应把锚点钉在当下，得到 %v", first)
	}

	s.UpdateAccount(aid, map[string]any{"cookies_exp": map[string]any{"hatch_sess": 1700000001}})
	second := s.GetAccount(aid)["expiry_anchor"]
	if numOr0(second) != numOr0(first) {
		t.Fatalf("锚点只应钉一次：%v -> %v", first, second)
	}

	s.UpdateAccount(aid, map[string]any{"cookies_exp": map[string]any{"hatch_vml": 1800000000}})
	got := s.GetAccount(aid)
	if got["expiry_anchor"] != nil {
		t.Fatalf("拿到 hatch_vml 真值后应清空锚点，得到 %v", got["expiry_anchor"])
	}
	if e := got["expires_at"]; !isPos(e) || posInt(e) != 1800000000 {
		t.Fatalf("expires_at 应为 hatch_vml 真值，得到 %v", e)
	}
}

func TestUpdateAccountSkipsNil(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	aid := a["id"].(string)
	s.UpdateAccount(aid, map[string]any{"label": "改名", "note": nil})
	got := s.GetAccount(aid)
	if got["label"] != "改名" {
		t.Fatalf("label 没更新: %v", got["label"])
	}
	if got["note"] != "" {
		t.Fatalf("nil 值应跳过而不是清空，得到 %v", got["note"])
	}
}

func TestListAccountsHidesCookiesAndAddsDerived(t *testing.T) {
	s, _ := newStore(t)
	s.AddAccount(map[string]any{
		"hatch_sess": "s", "hatch_gw": "g", "hatch_vml": "v", "hatch_native_auth_device": "d",
	}, "A", map[string]any{"hatch_vml": 1700086400})
	list := s.ListAccounts()
	if len(list) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(list))
	}
	item := list[0]
	if _, ok := item["cookies"]; ok {
		t.Fatal("list_accounts 不能把 cookies 吐给前端")
	}
	if item["cookie_count"] != 4 {
		t.Errorf("cookie_count=%v", item["cookie_count"])
	}
	if item["essential_ok"] != true {
		t.Errorf("essential_ok 应为 true")
	}
	if item["expires_estimated"] != false {
		t.Errorf("有 hatch_vml 真值时不应标 estimated")
	}
	if posInt(item["expires_at"]) != 1700086400 {
		t.Errorf("expires_at 应被 hatch_vml 覆盖")
	}
}

func TestStatsBuckets(t *testing.T) {
	s, _ := newStore(t)
	a := s.AddAccount(map[string]any{"hatch_sess": "1"}, "A", nil)
	b := s.AddAccount(map[string]any{"hatch_sess": "2"}, "B", nil)
	c := s.AddAccount(map[string]any{"hatch_sess": "3"}, "C", nil)
	s.Mark(a["id"].(string), true, "")
	s.Mark(b["id"].(string), false, "坏")
	s.UpdateAccount(c["id"].(string), map[string]any{"enabled": false})

	st := s.Stats()
	if st["total"] != 3 || st["enabled"] != 2 || st["disabled"] != 1 {
		t.Fatalf("计数不对: %v", st)
	}
	if st["healthy"] != 1 || st["error"] != 1 {
		t.Fatalf("健康/异常计数不对: %v", st)
	}
}

func TestTaskLifecycle(t *testing.T) {
	s, _ := newStore(t)
	t1 := s.CreateTask("image", "画一只猫")
	if tid, _ := t1["id"].(string); len(tid) != 5+20 || tid[:5] != "task_" {
		t.Fatalf("任务 id 形态不对: %q", tid)
	}
	if t1["status"] != "queued" || t1["object"] != "task" || t1["result"] != nil || t1["error"] != nil {
		t.Fatalf("初始任务形态不对: %v", t1)
	}
	s.CreateTask("video", "第二段")

	updated := s.UpdateTask(t1["id"].(string), map[string]any{"status": "completed", "result": map[string]any{"url": "/v1/media/x.webp"}})
	if updated["status"] != "completed" {
		t.Fatal("update_task 没生效")
	}
	if s.UpdateTask("task_不存在", map[string]any{"status": "x"}) != nil {
		t.Fatal("不存在的任务应返回 nil")
	}
	list := s.ListTasks(1)
	if len(list) != 1 {
		t.Fatalf("limit 没生效: %d", len(list))
	}
	if removed := s.ClearTasks(1); removed != 1 {
		t.Fatalf("clear_tasks(keep=1) 应删 1 条，得到 %d", removed)
	}
	if len(s.ListTasks(-1)) != 1 {
		t.Fatal("clear 后应剩 1 条")
	}
}

// 落盘必须是原子写的合法 JSON，且中文与 <>& 不被转义（ensure_ascii=False / 无 HTML 转义）。
func TestPersistedFileFormat(t *testing.T) {
	s, cfg := newStore(t)
	s.AddAccount(map[string]any{"hatch_sess": "<a&b>"}, "周杰伦 <Jay>", map[string]any{"hatch_vml": 1700086400})
	raw, err := os.ReadFile(cfg.AccountsFile())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"周杰伦 <Jay>", "<a&b>"} {
		if !contains(text, want) {
			t.Fatalf("落盘内容把 %q 转义了：\n%s", want, text)
		}
	}
	for _, bad := range []string{"\\u5468", "\\u003c", "\\u003e", "\\u0026"} {
		if contains(text, bad) {
			t.Fatalf("出现了 Python 不会有的转义形式 %q：\n%s", bad, text)
		}
	}
	// Python 的 json.dump(indent=1) 每层缩进 1 个空格：数组元素在 1 格、对象键在 2 格。
	if !contains(text, "\n {\n  \"id\": ") {
		t.Fatalf("indent 应为 1 个空格：\n%s", text)
	}
	// 键序必须等于 Python add_account 的插入顺序
	want := []string{"id", "label", "cookies", "cookies_exp", "expires_at", "expiry_anchor",
		"enabled", "created_at", "last_used", "last_keepalive", "use_count", "ok", "note", "synced_at"}
	prev := -1
	for _, k := range want {
		i := indexOf(text, `"`+k+`":`)
		if i < 0 {
			t.Fatalf("缺少键 %s：\n%s", k, text)
		}
		if prev >= 0 && i < prev {
			t.Fatalf("键序与 Python 不一致：%s 排在了前面\n%s", k, text)
		}
		prev = i
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
