package pyval

import "testing"

// 这份表按 CPython Lib/json/encoder.py 的两条正则写死：
//
//	ESCAPE       = re.compile(r'[\x00-\x1f\\"\b\f\n\r\t]')  # ensure_ascii=False：只转控制符、引号、反斜杠
//	ESCAPE_ASCII = re.compile(r'([\\"]|[^\ -~])')           # ensure_ascii=True ：「空格~波浪号」之外全转
//	ESCAPE_DCT   = 上面那些具名转义 + 0x00–0x1f 的 \uXXXX（小写十六进制）
//	表里没有又仍被 ESCAPE_ASCII 命中的：BMP 内 → \uXXXX，增补平面 → 一对代理项
//
// 两条正则的差集正好是 0x7F–0xFF 与 0x10000+：所以 DEL(0x7F) 与中文在 True 下转义、False 下原样，
// 而 0x00–0x1f 两种模式都转。< > & 一律不转（那是 fastapi 的 _html_safe_json 自己补的）。
func TestDumpsStringEscaping(t *testing.T) {
	cases := []struct {
		in    string
		ascii string // ensure_ascii=True
		plain string // ensure_ascii=False
	}{
		{`a b~c.d_e#f`, `"a b~c.d_e#f"`, `"a b~c.d_e#f"`},
		{`<tag> & "q" \ x`, `"<tag> & \"q\" \\ x"`, `"<tag> & \"q\" \\ x"`},
		{"猫 100%", "\"\\u732b 100%\"", "\"猫 100%\""},
		{"é", "\"\\u00e9\"", "\"é\""},
		{"😀", "\"\\ud83d\\ude00\"", "\"😀\""},
		{"\x00\x1f\x7f", "\"\\u0000\\u001f\\u007f\"", "\"\\u0000\\u001f\x7f\""},
		{"\b\t\n\r\f", `"\b\t\n\r\f"`, `"\b\t\n\r\f"`},
	}
	for _, c := range cases {
		if got := Dumps(c.in, true, false); got != c.ascii {
			t.Errorf("ensure_ascii=True in=%q go=%q py=%q", c.in, got, c.ascii)
		}
		if got := Dumps(c.in, false, false); got != c.plain {
			t.Errorf("ensure_ascii=False in=%q go=%q py=%q", c.in, got, c.plain)
		}
	}
}

// sort_keys=True 会递归排序（幂等哈希那一路就靠它）；ensure_ascii 与它互不影响。
func TestDumpsSortKeysRecursive(t *testing.T) {
	v := map[string]any{"b": map[string]any{"z": 1, "a": 2}, "a": []any{map[string]any{"y": 1, "x": 2}}}
	want := `{"a": [{"x": 2, "y": 1}], "b": {"a": 2, "z": 1}}`
	if got := Dumps(v, false, true); got != want {
		t.Errorf("sort_keys go=%q py=%q", got, want)
	}
	// 默认分隔符是 ", " 与 ": "，不是 Go 的紧凑写法。
	if got := Dumps([]any{1, "猫"}, false, false); got != `[1, "猫"]` {
		t.Errorf("分隔符 go=%q", got)
	}
	if got := Dumps([]any{1, "猫"}, true, false); got != `[1, "\u732b"]` {
		t.Errorf("ensure_ascii go=%q", got)
	}
}
