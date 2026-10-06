package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"muse2api-go/internal/store"
)

type goldenJS struct {
	Consts  []jsEntry `json:"consts"`
	Concats []jsEntry `json:"concats"`
	Named   []struct {
		Name string   `json:"name"`
		B64  string   `json:"b64"`
		List []string `json:"list"`
	} `json:"named"`
}

type jsEntry struct {
	Line int    `json:"line"`
	B64  string `json:"b64"`
}

type fmtRow struct {
	Line      int    `json:"line"`
	N         int    `json:"n"`
	Tmpl      string `json:"tmpl"`
	Formatted string `json:"formatted"`
}

const varNUL = "\x00VAR:"

func loadGolden(t *testing.T) goldenJS {
	t.Helper()
	raw, err := os.ReadFile("testdata/engine_js.json")
	if err != nil {
		t.Skip("缺金样本 testdata/engine_js.json:", err)
	}
	var g goldenJS
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func decode(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	return string(b)
}

// stripVars 把拼接模板里的 \x00VAR:名字\x00 占位符去掉，得到"纯字面量拼起来"的串。
func stripVars(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], varNUL) {
			if j := strings.IndexByte(s[i:], 0); j >= 0 {
				i += j + 1
				continue
			}
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isConcatFragment(t *testing.T, text string, line int, g goldenJS) bool {
	for _, c := range g.Concats {
		if c.Line != line {
			continue
		}
		stripped := stripVars(decode(t, c.B64))
		if stripped != text && strings.Contains(stripped, text) {
			return true
		}
	}
	return false
}

// TestEveryPythonJSLiteralHasExactlyOneGoTwin 是双向覆盖检查：
// Python 侧每条独立 JS 必须能在 Go 里找到逐字节相同的常量；
// Go 侧每个常量也必须来自 Python 抽取结果 —— 防止手改 js.go 或私自加一段 JS。
func TestEveryPythonJSLiteralHasExactlyOneGoTwin(t *testing.T) {
	g := loadGolden(t)

	byValue := map[string][]string{}
	for name, val := range jsCatalog {
		byValue[val] = append(byValue[val], name)
	}

	seen := map[string]bool{}
	fragments := 0
	var missing []string
	duplicates := 0
	for _, c := range g.Consts {
		if seen[c.B64] {
			continue
		}
		seen[c.B64] = true
		text := decode(t, c.B64)
		names := byValue[text]
		if len(names) == 0 {
			if isConcatFragment(t, text, c.Line, g) {
				fragments++ // 拼接模板的组成字面量，不算独立常量
				continue
			}
			missing = append(missing, fmt.Sprintf("engine.py:%d: %s", c.Line, clip(text, 60)))
			continue
		}
		if len(names) > 1 {
			duplicates++
		}
	}
	if len(missing) > 0 {
		t.Errorf("这些 Python JS 在 Go 侧没有逐字节对应的常量:\n  %s", strings.Join(missing, "\n  "))
	}

	// 反向：Go 常量必须都能在抽取结果里找到原文。
	goldenSet := map[string]bool{}
	for _, c := range g.Consts {
		goldenSet[decode(t, c.B64)] = true
	}
	for name, val := range jsCatalog {
		if !goldenSet[val] {
			t.Errorf("jsCatalog[%s] 不是 Python 抽取到的原文，疑似手改", name)
		}
	}
	t.Logf("Go 常量 %d 条；Python 去重后 %d 条；拼接片段 %d 条；被多处复用的串 %d 条",
		len(jsCatalog), len(seen), fragments, duplicates)
}

// TestAssembledTemplatesMatchPython 还原 engine.py 里「字面量 + ATT_SEL」的拼接：
// Go 拼出来的整串必须与 Python 运行时的文本逐字节相同。
func TestAssembledTemplatesMatchPython(t *testing.T) {
	g := loadGolden(t)
	if len(g.Concats) != len(jsAssembled) {
		t.Fatalf("拼接模板数量不一致：Python=%d Go=%d", len(g.Concats), len(jsAssembled))
	}
	for _, c := range g.Concats {
		want := strings.ReplaceAll(decode(t, c.B64), varNUL+"ATT_SEL\x00", attSel)
		if want == decode(t, c.B64) {
			t.Fatalf("拼接模板里没有已知变量: line=%d", c.Line)
		}
		found := ""
		for name, fn := range jsAssembled {
			if fn() == want {
				found = name
			}
		}
		if found == "" {
			t.Errorf("Python line %d 的拼接模板在 Go 侧还原不出来:\n%s", c.Line, clip(want, 200))
			continue
		}
		t.Logf("%s == Python line %d 的拼接结果（%d 字节）", found, c.Line, len(want))
	}
}

// TestPyFormatParity 用真 Python 的 % 格式化结果当金样本，验证 Go 的 pyFormat。
func TestPyFormatParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/engine_js_fmt.jsonl")
	if err != nil {
		t.Skip("缺 engine_js_fmt.jsonl:", err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r fmtRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		tmpl := decode(t, r.Tmpl)
		want := decode(t, r.Formatted)
		args := make([]string, r.N)
		for i := range args {
			args[i] = "ARG" + strconv.Itoa(i+1)
		}
		got, err := pyFormat(tmpl, args...)
		if err != nil {
			t.Errorf("Python line %d: pyFormat 报错 %v（Python 是正常产出的）", r.Line, err)
			continue
		}
		if got != want {
			t.Errorf("Python line %d 格式化结果不一致\n got=%s\nwant=%s", r.Line, clip(got, 120), clip(want, 120))
		}
		if _, ok := lookupCatalog(tmpl); !ok {
			t.Errorf("Python line %d 的模板不在 jsCatalog 里", r.Line)
		}
		count++
	}
	t.Logf("比对 %d 条含 %%s 的模板，全部一致", count)
}

func lookupCatalog(text string) (string, bool) {
	for name, v := range jsCatalog {
		if v == text {
			return name, true
		}
	}
	return "", false
}

// TestNamedConstantsMatchPython 检查两条抓不到 JS 关键词的具名常量。
func TestNamedConstantsMatchPython(t *testing.T) {
	g := loadGolden(t)
	if len(g.Named) == 0 {
		t.Fatal("金样本里没有 named 条目")
	}
	checked := 0
	for _, n := range g.Named {
		switch n.Name {
		case "ATT_SEL":
			if got := decode(t, n.B64); got != attSel {
				t.Errorf("ATT_SEL 不一致: python=%q go=%q", got, attSel)
			}
			checked++
		case "ESSENTIAL_COOKIES":
			if strings.Join(n.List, ",") != strings.Join(store.EssentialCookies, ",") {
				t.Errorf("核心 cookie 名单不一致: python=%v go=%v", n.List, store.EssentialCookies)
			}
			checked++
		}
	}
	if checked != len(g.Named) {
		t.Errorf("金样本里有未比对的具名常量，请补测试")
	}
}

func TestPyFormatSemantics(t *testing.T) {
	cases := []struct {
		name, tmpl string
		args       []string
		want       string
		wantErr    string
	}{
		{"单个 %s", "a%sb", []string{"X"}, "aXb", ""},
		{"两个 %s 按顺序", "%s-%s", []string{"1", "2"}, "1-2", ""},
		{"%% 还原成 %", "100%% 用 %s", []string{"完"}, "100% 用 完", ""},
		{"缺参数", "%s%s", []string{"only"}, "", "not enough arguments"},
		{"多参数", "%s", []string{"a", "b"}, "", "not all arguments"},
		{"未知动词", "进度 %d", []string{"1"}, "", "unsupported format character"},
		{"裸 % 结尾", "abc%", nil, "", "不支持的格式化"},
		{"没有占位符", "plain", nil, "plain", ""},
		{"参数里含 % 不再解析", "%s", []string{"100%"}, "100%", ""},
	}
	for _, c := range cases {
		got, err := pyFormat(c.tmpl, c.args...)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: 期望错误含 %q，得到 err=%v got=%q", c.name, c.wantErr, err, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 不该报错: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got=%q want=%q", c.name, got, c.want)
		}
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
