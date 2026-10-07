package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// runGoldenSequence 用固定 uuid/时间走一遍与 tools/golden/golden.py（真 Python store.py）
// 完全相同的调用序列，产物落在 dir 下。
func runGoldenSequence(t *testing.T, dir string) (stats map[string]any, pickedSame bool) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	origUUID, origUnix, origFloat := uuidHex, nowUnix, nowFloat
	t.Cleanup(func() { uuidHex, nowUnix, nowFloat = origUUID, origUnix, origFloat })

	n := 0
	uuidHex = func(k int) string {
		n++
		return (string(rune('0'+n)) + strings.Repeat("0", 63))[:k]
	}
	nowUnix = func() int { return 1700000000 }
	nowFloat = func() float64 { return 1700000000.5 }

	cfg := fakeCfg{dir: dir}
	s := New(cfg)

	a1 := s.AddAccount(map[string]any{
		"hatch_sess": "s1", "hatch_gw": "g1", "hatch_vml": "v1",
		"hatch_native_auth_device": "d1", "extra": "<a&b> 中文",
	}, "测试 <A&B> 周杰伦", map[string]any{
		"hatch_vml": "1700086400", "hatch_gw": 1790000000, "bad": "abc", "zero": 0,
	})
	a2 := s.AddAccount(map[string]any{"hatch_sess": "s2"}, "", map[string]any{"hatch_sess": 1700090000})
	s.Mark(a1["id"].(string), true, "会话有效")
	s.TouchKeepalive(a2["id"].(string), nil, "保活未确认（保留账号状态）: HTTP 503")
	s.UpdateAccount(a1["id"].(string), map[string]any{
		"cookies_exp": map[string]any{"hatch_vml": 1700172800},
		"quota":       map[string]any{"plan": "Free plan", "weekly_used_pct": 12},
	})
	picked := s.PickAccount(a1["id"].(string), true, false, "")
	t1 := s.CreateTask("image", "画一只猫")
	s.UpdateTask(t1["id"].(string), map[string]any{
		"status": "completed",
		"result": map[string]any{"url": "/v1/media/x.webp", "kind": "image"},
	})
	return s.Stats(), picked["id"] == a1["id"]
}

// TestGoldenDump 只是把 Go 侧的金样本打成 base64 便于人工/脚本比对。
func TestGoldenDump(t *testing.T) {
	stats, pickedSame := runGoldenSequence(t, ".tmp/golden")
	t.Logf("PICKED=%v STATS=%v", pickedSame, stats)
	acctRaw, _ := os.ReadFile(".tmp/golden/accounts.json")
	taskRaw, _ := os.ReadFile(".tmp/golden/tasks.json")
	t.Log("ACCOUNTS_B64=" + base64.StdEncoding.EncodeToString(acctRaw))
	t.Log("TASKS_B64=" + base64.StdEncoding.EncodeToString(taskRaw))
}

// TestGoldenAgainstPython 是存储层的契约测试：Go 写出的 accounts.json / tasks.json
// 必须与真 Python store.py 在相同输入下的产物语义一致、顶层键序一致、格式风格一致。
//
// 唯一被允许的差别是嵌套 map 的键序（Go 的 map 无序，Python 的 dict 保序）——
// 那是 JSON 对象成员顺序，两个读方都按键取值，不影响任何行为（键序类差异不编号，见 README 差异一节说明）。
func TestGoldenAgainstPython(t *testing.T) {
	dir := t.TempDir()
	stats, pickedSame := runGoldenSequence(t, dir)

	pyAccounts, err := os.ReadFile("testdata/accounts.python.json")
	if err != nil {
		t.Skip("缺少 testdata 金样本:", err)
	}
	pyTasks, err := os.ReadFile("testdata/tasks.python.json")
	if err != nil {
		t.Skip("缺少 testdata 金样本:", err)
	}
	goAccounts, _ := os.ReadFile(filepath.Join(dir, "accounts.json"))
	goTasks, _ := os.ReadFile(filepath.Join(dir, "tasks.json"))

	if !pickedSame {
		t.Error("pick_account 的粘性复用结果与 Python 不一致")
	}
	// Python 侧同序列的 stats()：{"disabled":0,"enabled":2,"error":0,"expired":0,"expiring":2,"healthy":1,"total":2}
	wantStats := map[string]int{"total": 2, "enabled": 2, "disabled": 0,
		"healthy": 1, "error": 0, "expiring": 2, "expired": 0}
	for k, v := range wantStats {
		if got, ok := stats[k].(int); !ok || got != v {
			t.Errorf("stats.%s=%v，Python 是 %d", k, stats[k], v)
		}
	}
	for _, c := range []struct {
		name      string
		py, goRaw []byte
	}{
		{"accounts.json", pyAccounts, goAccounts},
		{"tasks.json", pyTasks, goTasks},
	} {
		if len(c.py) != len(c.goRaw) {
			t.Errorf("%s 字节数不同：py=%d go=%d（缩进/转义/浮点格式可能变了）",
				c.name, len(c.py), len(c.goRaw))
		}
		if err := deepEqualSortedKeys(c.py, c.goRaw, c.name); err != nil {
			t.Errorf("%s 语义不一致: %v", c.name, err)
			continue
		}
		pk, gk := topLevelKeySeqs(c.py), topLevelKeySeqs(c.goRaw)
		if strings.Join(pk, "\n") != strings.Join(gk, "\n") {
			t.Errorf("%s 顶层键序不一致:\n py=%v\n go=%v", c.name, pk, gk)
			continue
		}
		if err := sameIndentProfile(c.py, c.goRaw); err != nil {
			t.Errorf("%s 缩进风格不一致: %v", c.name, err)
			continue
		}
		t.Logf("%s 与真 Python 产物一致（%d 字节，嵌套键序除外）", c.name, len(c.py))
	}
}

// deepEqualSortedKeys 递归比较两个 JSON，忽略所有对象层的键顺序。
func deepEqualSortedKeys(py, goRaw []byte, name string) error {
	var a, b any
	if err := json.Unmarshal(py, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(goRaw, &b); err != nil {
		return err
	}
	return compare(a, b, name)
}

func compare(a, b any, path string) error {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: 类型不同 %T vs %T", path, a, b)
		}
		if len(av) != len(bv) {
			return fmt.Errorf("%s: 键数 %d vs %d", path, len(av), len(bv))
		}
		keys := make([]string, 0, len(av))
		for k := range av {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bv2, ok := bv[k]
			if !ok {
				return fmt.Errorf("%s.%s: Go 侧缺这个键", path, k)
			}
			if err := compare(av[k], bv2, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return fmt.Errorf("%s: 数组长度/类型不同", path)
		}
		for i := range av {
			if err := compare(av[i], bv[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			return fmt.Errorf("%s: %v vs %v", path, a, b)
		}
		return nil
	}
}

var keyLine = regexp.MustCompile(`^( *)"(id|label|cookies|cookies_exp|expires_at|expiry_anchor|enabled|created_at|last_used|last_keepalive|use_count|ok|note|synced_at|checked_at|quota|object|kind|prompt|status|updated_at|result|error)":`)

// topLevelKeySeqs 按缩进层级把每个对象块里的键序列抽出来。
func topLevelKeySeqs(raw []byte) []string {
	var seqs []string
	var cur []string
	for _, line := range strings.Split(string(raw), "\n") {
		m := keyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if len(m[1]) == 2 { // 账号/任务对象体在 2 格缩进
			cur = append(cur, m[2])
			continue
		}
		if len(m[1]) == 1 && len(cur) > 0 {
			seqs = append(seqs, strings.Join(cur, ","))
			cur = nil
		}
	}
	if len(cur) > 0 {
		seqs = append(seqs, strings.Join(cur, ","))
	}
	return seqs
}

// sameIndentProfile 校验逐行前导空格数一致（等价于缩进宽度与嵌套层级一致）。
func sameIndentProfile(py, goRaw []byte) error {
	pl, gl := strings.Split(string(py), "\n"), strings.Split(string(goRaw), "\n")
	if len(pl) != len(gl) {
		return fmt.Errorf("行数 %d vs %d", len(pl), len(gl))
	}
	for i := range pl {
		if indentOf(pl[i]) != indentOf(gl[i]) {
			return fmt.Errorf("第 %d 行缩进不同: %q vs %q", i, pl[i], gl[i])
		}
	}
	return nil
}

func indentOf(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}
