package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pyJSONErr 的分支表：右边的 (msg, pos) 全部取自线上录的那八条金样本，
// 不是照着 Go 的习惯编的 —— pos 数的是字符，不是字节。
func TestPyJSONErrMatchesOracle(t *testing.T) {
	cases := []struct {
		body string
		msg  string
		pos  int
	}{
		{`{"a"}`, "Expecting ':' delimiter", 4},
		{`{"a":}`, "Expecting value", 5},
		{`[1 2]`, "Expecting ',' delimiter", 3},
		{`{"a": 1, }`, "Expecting property name enclosed in double quotes", 9},
		{`["abc`, "Unterminated string starting at", 1},
		{`{} []`, "Extra data", 3},
		{" ", "Expecting value", 1},
		{"{坏 json", "Expecting property name enclosed in double quotes", 1}, // 按字符数：不是 3
		// 扫描器的其余分支：前三条是内部补强，后三条（escape / control / unicode）
		// 分别由 gen_bad_escape、gen_bad_control、gen_bad_uni 三条金样本背书。
		{`[1,`, "Expecting value", 3},
		{`{"a": 1`, "Expecting ',' delimiter", 7},
		{`{"a" 1}`, "Expecting ':' delimiter", 5},
		{`nul`, "Expecting value", 0},
		{`{"a": "b",}`, "Expecting property name enclosed in double quotes", 10},
		{`["a" "b"]`, "Expecting ',' delimiter", 5},
		{`{"a": "b\q"}`, "Invalid \\escape", 8},
		{`{"a": "b` + "\x07" + `c"}`, "Invalid control character at", 8},
		{`{"a": "x\u12g"}`, "Invalid \\uXXXX escape", 9},
	}
	for _, c := range cases {
		msg, pos := pyJSONErr(c.body)
		if msg != c.msg || pos != c.pos {
			t.Errorf("body=%q go=(%q,%d) want=(%q,%d)", c.body, msg, pos, c.msg, c.pos)
		}
	}
	// 合法 JSON 不该报错。
	for _, ok := range []string{`{}`, `[]`, `1`, `"s"`, `null`, `{"a":[1,{"b":2}]}`, `  {}  `} {
		if msg, pos := pyJSONErr(ok); msg != "" {
			t.Errorf("合法 body %q 被判成错误：(%q,%d)", ok, msg, pos)
		}
	}
}

// contentTypeIsJSON 就是 FastAPI 那句「maintype 是 application 且 subtype 是 json 或 +json」；
// 缺头也是 false（strict_content_type 默认 True）—— 五条 gen_ctype_* 金样本把边界钉住了。
func TestContentTypeIsJSON(t *testing.T) {
	yes := []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON",
		"application/vnd.api+json", "application/hal+json", "application/json;charset=UTF-8"}
	no := []string{"", "text/plain", "application/x-www-form-urlencoded", "text/json",
		"multipart/form-data", "application", "json", "application/jsonp"}
	for _, c := range yes {
		if !contentTypeIsJSON(c) {
			t.Errorf("%q 应当按 JSON 解析", c)
		}
	}
	for _, c := range no {
		if contentTypeIsJSON(c) {
			t.Errorf("%q 不该按 JSON 解析（body 要当 bytes）", c)
		}
	}
}

// bytesRepr 是 Python 的 repr(b'...')：gen_noctype_empty 那条金样本里 'input': b'{}' 就是它。
func TestBytesRepr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"{}", `b'{}'`},
		{"", `b''`},
		{"it's", `b"it's"`},         // 有单引号且无双引号 → 改用双引号
		{`say "hi"`, `b'say "hi"'`}, // 反过来
		{"a'b\"c", `b'a\'b"c'`},     // 两者都有 → 留单引号并转义
		{"tab\there", `b'tab\there'`},
		{"猫", `b'\xe7\x8c\xab'`}, // 非 ASCII 逐字节 \xNN
		{"\x07bell", `b'\x07bell'`},
	}
	for _, c := range cases {
		if got := bytesRepr(pyBytes(c.in)); got != c.want {
			t.Errorf("bytesRepr(%q) go=%s want=%s", c.in, got, c.want)
		}
	}
}

// 两条 1.5.4 的新路由在 1.5.3 上不存在，金样本录不到 —— 断言照上游
// tests/test_media_delete.py 的五条 + Python 源码里的几个细节来写。
func TestMediaDeleteRoutes(t *testing.T) {
	a, mediaDir := testApp(t, "cap-key", time.Unix(1700000000, 0))
	post := func(body string, ctype string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/admin/media/delete", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer cap-key")
		if ctype != "" {
			req.Header.Set("content-type", ctype)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		return rec
	}
	mk := func(name string) string {
		p := filepath.Join(mediaDir, name)
		if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	t.Run("上游用例2：批量删真的删掉", func(t *testing.T) {
		f1, f2 := mk("sample1.webp"), mk("sample2.mp4")
		rec := post(`{"names":["sample1.webp"]}`, "application/json")
		if rec.Code != 200 || rec.Body.String() != `{"removed":1,"errors":[]}` {
			t.Fatalf("go=%d %s", rec.Code, rec.Body.String())
		}
		if exists(f1) || !exists(f2) {
			t.Error("只该删掉点名的那一个")
		}
		os.Remove(f2)
	})

	t.Run("上游用例3：穿越路径被挡且不删除任何东西", func(t *testing.T) {
		// 文件根本不存在的名字也要报错而不是静默跳过 —— 非法名字与「没有这个文件」是两条分支
		rec := post(`{"names":["../app.py","..\\app.py"]}`, "application/json")
		if rec.Code != 200 {
			t.Fatalf("go=%d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"removed":0`) ||
			!strings.Contains(rec.Body.String(), `非法文件名: ../app.py`) {
			t.Errorf("响应体不同：%s", rec.Body.String())
		}
	})

	t.Run("names 给成字符串等价于单元素数组", func(t *testing.T) {
		p := mk("solo.webp")
		rec := post(`{"names":"solo.webp"}`, "application/json")
		if rec.Body.String() != `{"removed":1,"errors":[]}` || exists(p) {
			t.Errorf("go=%s 文件还在=%v", rec.Body.String(), exists(p))
		}
	})

	t.Run("names 不是数组也不是字符串 → 400", func(t *testing.T) {
		rec := post(`{"names":5}`, "application/json")
		if rec.Code != 400 || rec.Body.String() != `{"detail":"names 必须是文件名数组"}` {
			t.Errorf("go=%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("文件不存在既不计数也不报错", func(t *testing.T) {
		rec := post(`{"names":["没有这个文件.webp"]}`, "application/json")
		if rec.Body.String() != `{"removed":0,"errors":[]}` {
			t.Errorf("go=%s，Python 是静默跳过", rec.Body.String())
		}
	})

	t.Run("目录名被 isfile 挡掉：静默跳过", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(mediaDir, "adir"), 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(mediaDir, "adir"))
		rec := post(`{"names":["adir"]}`, "application/json")
		if rec.Body.String() != `{"removed":0,"errors":[]}` {
			t.Errorf("go=%s", rec.Body.String())
		}
	})

	t.Run("非法名字里的非字符串按 Python str() 写", func(t *testing.T) {
		rec := post(`{"names":[null,true,123,1.5,["x"]]}`, "application/json")
		got := rec.Body.String()
		for _, want := range []string{"非法文件名: None", "非法文件名: True",
			"非法文件名: 123", `非法文件名: ['x']`} {
			if !strings.Contains(got, want) {
				t.Errorf("响应里应含 %q，得到 %s", want, got)
			}
		}
		if !strings.Contains(got, `非法文件名: '1.5'`) && !strings.Contains(got, "非法文件名: 1.5") {
			t.Errorf("浮点名字写法不对：%s", got)
		}
	})

	t.Run("没给 body 等价于空数组", func(t *testing.T) {
		rec := post("", "application/json")
		if rec.Code != 200 || rec.Body.String() != `{"removed":0,"errors":[]}` {
			t.Errorf("go=%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("空数组 errors 必须是 [] 不是 null", func(t *testing.T) {
		rec := post(`{"names":[]}`, "application/json")
		if got := rec.Body.String(); got != `{"removed":0,"errors":[]}` {
			t.Errorf("go=%s", got)
		}
	})

	t.Run("body 校验层与别的 POST 路由同规则", func(t *testing.T) {
		if rec := post(`[]`, "application/json"); rec.Code != 422 {
			t.Errorf("数组 body go=%d %s", rec.Code, rec.Body.String())
		}
		if rec := post(`{坏`, "application/json"); rec.Code != 422 {
			t.Errorf("坏 JSON go=%d %s", rec.Code, rec.Body.String())
		}
		// Content-Type 不是 JSON → body 是 bytes → detail 列表序列化不了 → 线上同款 500
		if rec := post(`{}`, ""); rec.Code != 500 {
			t.Errorf("无 Content-Type go=%d，应 500（%s）", rec.Code, rec.Body.String())
		}
		// 没带密钥：auth 依赖先失败，401（与 Python 的依赖求解顺序一致）
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/admin/media/delete", strings.NewReader(`{}`))
		a.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("无密钥 go=%d，应 401", rec.Code)
		}
	})

	t.Run("上游用例4、5：单删路由", func(t *testing.T) {
		p := mk("sample3.mp4")
		req := httptest.NewRequest("DELETE", "/admin/media/sample3.mp4", nil)
		req.Header.Set("Authorization", "Bearer cap-key")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != `{"status":"ok","deleted":"sample3.mp4"}` {
			t.Fatalf("go=%d %s", rec.Code, rec.Body.String())
		}
		if exists(p) {
			t.Error("文件没被删掉")
		}
		// 不存在 → 404 文件不存在
		req = httptest.NewRequest("DELETE", "/admin/media/nonexistent.webp", nil)
		req.Header.Set("Authorization", "Bearer cap-key")
		rec = httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != 404 || rec.Body.String() != `{"detail":"文件不存在"}` {
			t.Errorf("go=%d %s", rec.Code, rec.Body.String())
		}
		// 穿越 → 400（与 GET /v1/media/{name} 同一句文案）
		req = httptest.NewRequest("DELETE", "/admin/media/..%2Fx", nil)
		req.Header.Set("Authorization", "Bearer cap-key")
		rec = httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != 400 && rec.Code != 404 {
			t.Errorf("穿越名 go=%d %s", rec.Code, rec.Body.String())
		}
	})
}

// Python 元组比较：1.5.4 的 has_update 用的是「远端版本元组 > 本地」。
func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.5.4", "1.5.3", true},
		{"1.5.3", "1.5.4", false},
		{"1.5.3", "1.5.3", false},
		{"1.5", "1.5.1", false}, // 前缀相同时短的小
		{"1.5.1", "1.5", true},
		{"v1.6.0-beta2", "1.5.9", true},
		{"", "1.0", false},
	}
	for _, c := range cases {
		if got := versionGT(parseVersion(c.a), parseVersion(c.b)); got != c.want {
			t.Errorf("%q > %q go=%v want=%v", c.a, c.b, got, c.want)
		}
	}
	// 数字段抽取：非数字分隔符全部忽略，"v10.2.0rc3" → 10,2,0,3
	if got := parseVersion("v10.2.0rc3"); !reflect.DeepEqual(got, []int64{10, 2, 0, 3}) {
		t.Errorf("parseVersion go=%v", got)
	}
	if got := parseVersion(""); len(got) != 0 {
		t.Errorf("空串应当是空元组，得到 %v", got)
	}
}
