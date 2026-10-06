package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"muse2api-go/internal/config"
	"muse2api-go/internal/engine"
	"muse2api-go/internal/store"
)

// 这份金样本是从 NAS 上**现役的 Python 实例**打出来的对外契约（tools/golden/golden_routes.py）。
// 口径（详见 tools/golden/README.md）：
//   - body 型：状态码 + content-type + 脱敏后的 JSON 深比较；
//   - shape 型：只比键集合与类型（账号数、任务列表是活的线上数据，值不可比）；
//   - head 型：只比响应头 —— 媒体文件内容由测试按线上同名/同尺寸/同 mtime 造出来，
//     于是 etag 与 last-modified 也是可比的，这一条同时钉住了 etag 算法和 mimetypes 判定。
func TestV1ReadOnlyContract(t *testing.T) {
	g := loadRoutesGolden(t)

	// 线上那两个媒体文件的精确 st_mtime（容器里 os.stat 取回的浮点秒），
	// 用来在本地复现同一个 etag：算法与 str(float) 的写法都要对上。
	seed := map[string]struct {
		mtime float64
		size  int64
	}{
		"2fefc0ec0931492da3529cca7cdec6be.webp": {1791212303.8032646, 115944},
		".gitkeep":                              {1791112752.6836884, 14},
	}
	a, mediaDir := testApp(t, "cap-key", time.Unix(1791264799, 0))
	for name, s := range seed {
		p := filepath.Join(mediaDir, name)
		if err := os.WriteFile(p, make([]byte, s.size), 0o644); err != nil {
			t.Fatal(err)
		}
		secs := int64(s.mtime)
		ts := time.Unix(secs, int64((s.mtime-float64(secs))*1e9))
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		mode string
		drop []string
	}{
		{"healthz", "body", []string{"time"}},
		{"readyz", "shape", nil},
		{"models_anon", "body", nil},
		{"models_bad", "body", nil},
		{"models_ok", "body", nil},
		{"media_missing", "body", nil},
		{"media_backslash", "body", nil},
		{"media_backslash_traversal", "body", nil},
		{"media_dots_only", "body", nil},
		{"media_enc_slash_only", "body", nil},
		{"media_space_name", "body", nil},
		{"media_traversal", "body", nil},
		{"media_dotdot_plain", "body", nil},
		{"media_utf8_name", "body", nil},
		{"media_real_0", "head", nil},
		{"media_real_1", "head", nil},
		{"v1_unmatched", "body", nil},
		{"admin_unmatched", "body", nil},
		{"cors_get", "body", nil},
		{"cors_options", "head", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := section(t, g, c.name)
			rec := do(a, row)
			wantStatus := int(num(row["status"]))
			if rec.Code != wantStatus {
				t.Fatalf("状态码 go=%d py=%d（go body=%s）", rec.Code, wantStatus, rec.Body.String())
			}
			wantHead := strMap(row["headers"])
			switch c.mode {
			case "body":
				if ct := rec.Header().Get("content-type"); ct != wantHead["content-type"] {
					t.Errorf("content-type go=%q py=%q", ct, wantHead["content-type"])
				}
				got := decode(rec.Body.Bytes())
				want := row["body"]
				for _, k := range c.drop {
					dropKey(got, k)
					dropKey(want, k)
				}
				if !jsonEquiv(want, got) {
					t.Errorf("响应体不同\n py=%v\n go=%s", row["body"], rec.Body.String())
				}
			case "shape":
				if !sameShape(shapeOf(t, row), decode(rec.Body.Bytes())) {
					t.Errorf("键集合/类型不同\n py=%v\n go=%s", row["body_shape"], rec.Body.String())
				}
			case "head":
				for k, want := range wantHead {
					if k == "vary" && !strings.Contains(rec.Header().Get("vary"), "Origin") {
						t.Errorf("vary 里应有 Origin，go=%q", rec.Header().Get("vary"))
						continue
					}
					if got := rec.Header().Get(k); got != want {
						t.Errorf("%s go=%q py=%q", k, got, want)
					}
				}
			}
			// CORS 头在三种模式下都该在（只要探针带了 Origin，或本来就是预检）。
			if ph := probeHeaders(row); ph["Origin"] != "" && c.mode != "shape" {
				if got := rec.Header().Get("access-control-allow-origin"); got != "*" {
					t.Errorf("access-control-allow-origin go=%q py=*", got)
				}
			}
		})
	}
}

// TestGoldenCasesAllAccountedFor 是进度台账：金样本里每一条用例都必须落在
// 「已回放」或「待实现（点名）」里，防止后面加路由时静默漏掉某条契约。
func TestGoldenCasesAllAccountedFor(t *testing.T) {
	g := loadRoutesGolden(t)
	replayed := map[string]bool{
		"healthz": true, "readyz": true,
		"models_anon": true, "models_bad": true, "models_ok": true,
		"media_missing": true, "media_backslash": true, "media_backslash_traversal": true,
		"media_dots_only": true, "media_enc_slash_only": true, "media_space_name": true,
		"media_traversal": true, "media_dotdot_plain": true, "media_utf8_name": true,
		"media_real_0": true, "media_real_1": true,
		"v1_unmatched": true, "admin_unmatched": true,
		"cors_get": true, "cors_options": true,
	}
	deferred := map[string]string{
		"openapi": "D3：内嵌 /openapi.json 静态文件",
		"docs":    "D3：内嵌 /docs",
		"index":   "D3：内嵌 admin.html", "admin_page": "D3：内嵌 admin.html",
		"cookie_helper":    "D3：内嵌 cookie 助手脚本（FileResponse 语义已由 media_real 验过）",
		"cookie_helper_dl": "D3：同上", "extension": "D3：扩展 zip 构建",
		"extension_files": "D3：扩展文件列表",
		// 其余按路由分组待实现，这里点名到组，避免"未知"混进来。
	}
	var missing []string
	for name, raw := range g {
		if name == "__meta__" || replayed[name] {
			continue
		}
		if _, ok := deferred[name]; ok {
			continue
		}
		row, _ := raw.(map[string]any)
		p, _ := row["path"].(string)
		switch {
		case strings.HasPrefix(p, "/admin/status"), strings.HasPrefix(p, "/admin/accounts"),
			strings.HasPrefix(p, "/admin/apikey"), strings.HasPrefix(p, "/admin/tasks"),
			strings.HasPrefix(p, "/admin/media"), strings.HasPrefix(p, "/admin/relogin"),
			strings.HasPrefix(p, "/admin/quota"), strings.HasPrefix(p, "/admin/keepalive"),
			strings.HasPrefix(p, "/admin/update"), strings.HasPrefix(p, "/admin/repo"),
			strings.HasPrefix(p, "/admin/nope"):
			deferred[name] = "D2：/admin 路由"
		case p == "/healthz" || p == "/readyz" || p == "/v1/models":
			t.Errorf("%s 既在台账又在 replayed？", name)
		case strings.HasPrefix(p, "/v1/images"), strings.HasPrefix(p, "/v1/videos"),
			strings.HasPrefix(p, "/v1/chat"), strings.HasPrefix(p, "/v1/responses"):
			deferred[name] = "D1：生成类路由与请求校验"
		default:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("这些金样本用例既没回放也没登记待实现：%v", missing)
	}
	t.Logf("已回放 %d 条，待实现 %d 条", len(replayed), len(deferred))
}

func loadRoutesGolden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/routes_golden.json")
	if err != nil {
		t.Skip("缺 routes_golden.json:", err)
	}
	var g map[string]any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func section(t *testing.T, g map[string]any, name string) map[string]any {
	t.Helper()
	row, ok := g[name].(map[string]any)
	if !ok {
		t.Fatalf("金样本里没有 %s", name)
	}
	return row
}

func sectionShape(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	s, ok := row["body_shape"].(map[string]any)
	if !ok {
		t.Fatalf("这一段没有 body_shape")
	}
	return s
}

// shapeOf：采集脚本对线上态接口记的是 body_shape，其余只有 body。
// 本地无法复现线上数据（账号数、任务数）的用例，把 body 折算成同构的 shape 再比。
func shapeOf(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	if s, ok := row["body_shape"].(map[string]any); ok {
		return s
	}
	d, ok := deriveShape(row["body"]).(map[string]any)
	if !ok {
		t.Fatalf("这一段既没有 body_shape 也不是对象 body")
	}
	return d
}

// deriveShape 与采集脚本里的 shape() 同构（__type__/__keys__/__items__/__n__/__first__）。
func deriveShape(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]any, 0, len(x))
		items := make(map[string]any, len(x))
		for k, sub := range x {
			keys = append(keys, k)
			items[k] = deriveShape(sub)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].(string) < keys[j].(string) })
		return map[string]any{"__type__": "object", "__keys__": keys, "__items__": items}
	case []any:
		var first any
		if len(x) > 0 {
			first = deriveShape(x[0])
		}
		return map[string]any{"__type__": "array", "__n__": float64(len(x)), "__first__": first}
	case string:
		return map[string]any{"__type__": "str", "__value__": x}
	case float64:
		return map[string]any{"__type__": "float", "__value__": x}
	case bool:
		return map[string]any{"__type__": "bool", "__value__": x}
	default:
		return map[string]any{"__type__": "NoneType", "__value__": nil}
	}
}

func testApp(t *testing.T, key string, now time.Time) (*App, string) {
	t.Helper()
	base := t.TempDir()
	cfg := &config.Config{BaseDir: base, Host: "0.0.0.0", Port: 18610,
		Chromium: "/usr/bin/chromium", CdpPort: 19210, HomeDir: base,
		ExtraPath: "/snap/bin", SiteURL: "https://muse.ai/", LoginWait: 60,
		ProfileRoot: filepath.Join(base, "profiles"), APIKey: key}
	cfg.EnsureDirs()
	a := NewApp(cfg, store.New(cfg), engine.NewEngine(cfg, nil, nil), &sync.Mutex{})
	a.now = func() time.Time { return now }
	return a, cfg.MediaDir()
}

func do(a *App, row map[string]any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	method, _ := row["method"].(string)
	path, _ := row["path"].(string)
	var body *strings.Reader
	if raw, ok := row["raw_body"].(string); ok {
		body = strings.NewReader(raw)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	switch auth, _ := row["auth"].(string); auth {
	case "bad":
		req.Header.Set("Authorization", "Bearer definitely-not-the-key")
	case "good":
		req.Header.Set("Authorization", "Bearer cap-key")
	}
	for k, v := range probeHeaders(row) {
		req.Header.Set(k, v)
	}
	a.ServeHTTP(rec, req)
	return rec
}

// probeHeaders 还原探针自带的请求头：目前只有两条 CORS 用例。
func probeHeaders(row map[string]any) map[string]string {
	if m, _ := row["method"].(string); m == "OPTIONS" {
		return map[string]string{"Origin": "https://muse.ai",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "authorization,content-type"}
	}
	if p, _ := row["path"].(string); p == "/v1/models" {
		return map[string]string{"Origin": "https://muse.ai"}
	}
	return nil
}

// --- 比对小工具 ---

func decode(b []byte) any {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return v
}

func strMap(v any) map[string]string {
	out := map[string]string{}
	if m, ok := v.(map[string]any); ok {
		for k, x := range m {
			s, _ := x.(string)
			out[k] = s
		}
	}
	return out
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func dropKey(v any, key string) {
	if m, ok := v.(map[string]any); ok {
		delete(m, key)
	}
}

// jsonEquiv：数字按 float 比（Go 的 json.Number 路径不存在，这里都是 float64），
// 字符串/布尔/nil 直接比，容器递归比。
func jsonEquiv(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for k, wv := range w {
			gv, has := g[k]
			if !has || !jsonEquiv(wv, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for i := range w {
			if !jsonEquiv(w[i], g[i]) {
				return false
			}
		}
		return true
	case float64:
		gf, ok := got.(float64)
		return ok && gf == w
	default:
		return reflect.DeepEqual(want, got)
	}
}

// sameShape：按金样本 body_shape 递归比键集合与类型，忽略值。
func sameShape(shape any, got any) bool {
	s, ok := shape.(map[string]any)
	if !ok {
		return true
	}
	switch t := s["__type__"]; t {
	case "object":
		gm, ok := got.(map[string]any)
		if !ok {
			return false
		}
		keys, _ := s["__keys__"].([]any)
		if len(keys) != len(gm) {
			return false
		}
		items, _ := s["__items__"].(map[string]any)
		for _, k := range keys {
			ks, _ := k.(string)
			v, has := gm[ks]
			if !has || !sameShape(items[ks], v) {
				return false
			}
		}
		return true
	case "array":
		gl, ok := got.([]any)
		if !ok {
			return false
		}
		if int(num(s["__n__"])) != len(gl) && int(num(s["__n__"])) != 0 {
			// 线上条目数与本地不同不视为不一致（任务/媒体是线上数据），
			// 但元素形状必须一致（除了双方都空）。
			if len(gl) > 0 {
				return sameShape(s["__first__"], gl[0])
			}
			return true
		}
		if len(gl) > 0 {
			return sameShape(s["__first__"], gl[0])
		}
		return true
	case nil:
		return got == nil || got == ""
	default:
		// 标量：只比 Python 类型名与 Go 侧的实际种类。
		want := t.(string)
		switch want {
		case "str":
			_, ok := got.(string)
			return ok
		case "int", "float":
			_, ok := got.(float64)
			return ok
		case "bool":
			_, ok := got.(bool)
			return ok
		case "NoneType":
			return got == nil
		}
		return true
	}
}
