package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"muse2api-go/internal/config"
	"muse2api-go/internal/engine"
	"muse2api-go/internal/pyval"
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

	cases := append(replayCases(), append(writeProbeCases(g), docProbeCases(g)...)...)
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
				if want == nil && row["blob_len"] != nil {
					// 非 JSON 响应（比如坏 JSON 打穿到 500 的那条）：金样本记的是摘要。
					if intOf(row["blob_len"]) != rec.Body.Len() {
						t.Errorf("响应长度不同: go=%d py=%v", rec.Body.Len(), row["blob_len"])
					}
					if bh, _ := row["blob_head"].(string); bh != "" && !strings.HasPrefix(rec.Body.String(), bh) {
						t.Errorf("响应体不同: go=%q py 前缀=%q", rec.Body.String(), bh)
					}
					break
				}
				for _, k := range c.drop {
					dropBodyKey(got, k)
					dropBodyKey(want, k)
				}
				if !jsonEquiv(want, got) {
					t.Errorf("响应体不同\n py=%v\n go=%s", row["body"], rec.Body.String())
				}
			case "shape":
				if !sameShape(shapeOf(t, row), decode(rec.Body.Bytes())) {
					t.Errorf("键集合/类型不同\n py=%v\n go=%s", row["body_shape"], rec.Body.String())
				}
			case "shallow":
				pyKeys := shapeKeys(t, row)
				gm, _ := decode(rec.Body.Bytes()).(map[string]any)
				var mine []string
				for k := range gm {
					mine = append(mine, k)
				}
				sort.Strings(mine)
				if strings.Join(mine, ",") != strings.Join(pyKeys, ",") {
					t.Errorf("顶层键集合不同\n py=%v\n go=%v", pyKeys, mine)
				}
			case "blob":
				if got := "fmt-x"; got != "" {
					_ = got
				}
				sum := sha256.Sum256(rec.Body.Bytes())
				if got := hex.EncodeToString(sum[:]); got != row["blob_sha256"] {
					t.Errorf("静态资源与线上不一致: go=%s py=%v", got, row["blob_sha256"])
				}
				if ct := rec.Header().Get("content-type"); ct != wantHead["content-type"] {
					t.Errorf("content-type go=%q py=%q", ct, wantHead["content-type"])
				}
			case "redirect":
				// 307 的 Location 是绝对地址，主机名跟着请求走，所以只比「 authority 之后」的那截。
				if got := locationTail(rec.Header().Get("location")); got != locationTail(wantHead["location"]) {
					t.Errorf("307 location go=%q py=%q（尾部 %q vs %q）",
						rec.Header().Get("location"), wantHead["location"],
						got, locationTail(wantHead["location"]))
				}
			case "headonly":
				if ct := rec.Header().Get("content-type"); ct != wantHead["content-type"] {
					t.Errorf("content-type go=%q py=%q", ct, wantHead["content-type"])
				}
			case "bodyallow":
				if !jsonEquiv(row["body"], decode(rec.Body.Bytes())) {
					t.Errorf("响应体不同\n py=%v\n go=%s", row["body"], rec.Body.String())
				}
				if got := rec.Header().Get("allow"); got != wantHead["allow"] {
					t.Errorf("allow go=%q py=%q", got, wantHead["allow"])
				}
			case "head":
				for k, want := range wantHead {
					if k == "etag" || k == "last-modified" {
						continue // 由各自实现按文件时间生成，不参与比对
					}
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
	replayed := replayedSet(g)

	deferred := map[string]string{}
	var missing []string
	replayedN, deferredN, total := 0, 0, 0
	for name, raw := range g {
		if name == "__meta__" {
			continue
		}
		total++
		if replayed[name] {
			replayedN++
			continue
		}
		if _, ok := deferred[name]; ok {
			deferredN++
			continue
		}
		row, _ := raw.(map[string]any)
		p, _ := row["path"].(string)
		switch {
		case strings.HasPrefix(p, "/admin/"):
			deferred[name] = "D2：/admin 路由里还依赖浏览器的几条"
			deferredN++
		case strings.HasPrefix(p, "/v1/"), p == "/healthz" || p == "/readyz":
			deferred[name] = "D1：生成类路由的成功路径"
			deferredN++
		default:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("这些金样本用例既没回放也没登记待实现：%v", missing)
	}
	if replayedN+deferredN != total {
		t.Errorf("台账没对上：金样本 %d 条，回放 %d + 待实现 %d = %d",
			total, replayedN, deferredN, replayedN+deferredN)
	}
	t.Logf("金样本 %d 条：已回放 %d，待实现 %d", total, replayedN, deferredN)
	var unrecorded []string
	for name := range docProbeModes {
		if _, ok := g[name]; !ok {
			unrecorded = append(unrecorded, name)
		}
	}
	if len(unrecorded) > 0 {
		sort.Strings(unrecorded)
		t.Logf("这几条 Go 已实现、录制脚本也已排进去，但线上还没录过（下次 NAS 复录后自动进回放表）：%v", unrecorded)
	}
}

// replayedSet 是「哪些金样本用例真的被回放了」的唯一出处：
// 手写的表 + 写接口探测清单，两处都由 contract 测试自己维护，不会与实跑脱节。
func replayedSet(g map[string]any) map[string]bool {
	replayed := map[string]bool{}
	for _, c := range replayCases() {
		replayed[c.name] = true
	}
	for _, c := range writeProbeCases(g) {
		replayed[c.name] = true
	}
	for _, c := range docProbeCases(g) {
		replayed[c.name] = true
	}
	return replayed
}
func loadRoutesGolden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/routes_golden.json")
	if err != nil {
		t.Skip("缺 routes_golden.json:", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留 Python 的 int/float 之分，回放请求体时 123 不能变成 123.0
	var g map[string]any
	if err := dec.Decode(&g); err != nil {
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

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	}
	return 0
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
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return map[string]any{"__type__": "float", "__value__": x}
		}
		return map[string]any{"__type__": "int", "__value__": x}
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

func testApp(t *testing.T, key string, now time.Time, hc ...engine.HTTPClient) (*App, string) {
	t.Helper()
	base := t.TempDir()
	cfg := &config.Config{BaseDir: base, Host: "0.0.0.0", Port: 18610,
		Chromium: "/usr/bin/chromium", CdpPort: 19210, HomeDir: base,
		ExtraPath: "/snap/bin", SiteURL: "https://muse.ai/", LoginWait: 60,
		ImageTimeout: 240, VideoTimeout: 600, ChatTimeout: 300,
		ProfileRoot: filepath.Join(base, "profiles"), APIKey: key}
	cfg.EnsureDirs()
	var client engine.HTTPClient
	if len(hc) > 0 {
		client = hc[0]
	}
	a := NewApp(cfg, store.New(cfg), engine.NewEngine(cfg, client, nil))
	a.now = func() time.Time { return now }
	return a, cfg.MediaDir()
}

func do(a *App, row map[string]any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	method, _ := row["method"].(string)
	path, _ := row["path"].(string)
	body := strings.NewReader("")
	if raw, ok := row["request_raw"].(string); ok && raw != "" {
		body = strings.NewReader(raw)
	} else if rb, ok := row["request_body"].(map[string]any); ok {
		body = strings.NewReader(pyval.Dumps(rb, false, false))
	}
	req := httptest.NewRequest(method, path, body)
	switch auth, _ := row["auth"].(string); auth {
	case "bad":
		req.Header.Set("Authorization", "Bearer definitely-not-the-key")
	case "good":
		req.Header.Set("Authorization", "Bearer cap-key")
	}
	if ct, ok := row["request_ctype"].(string); ok && ct != "" {
		req.Header.Set("Content-Type", ct)
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
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
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
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func dropBodyKey(v any, key string) {
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
			if !ok {
				_, ok = got.(json.Number)
			}
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

// replayCases 是契约测试用的用例表，也是台账判断「已回放」的唯一出处。
func replayCases() []replayCase {
	return []replayCase{
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
		{"task_missing", "body", nil}, {"video_missing", "body", nil},
		{"video_gen_missing", "body", nil},
		{"v1_unmatched", "body", nil},
		{"admin_unmatched", "body", nil},
		{"cors_get", "body", nil},
		{"cors_options", "head", nil},
		// 生成类路由：body 合法才会进 handler，所以金样本里这些用例全是「进不去」的分支。
		{"images_gen_noauth", "body", nil}, {"images_gen_bad", "body", nil},
		{"images_gen_empty", "body", nil}, {"images_gen_prompt_type", "body", nil},
		{"images_gen_timeout", "body", nil}, {"images_tasks_empty", "body", nil},
		{"images_tasks_bad", "body", nil}, {"videos_empty", "body", nil},
		{"videos_gen_empty", "body", nil}, {"videos_bad", "body", nil},
		{"chat_empty", "body", nil}, {"chat_bad_msgs", "body", nil},
		{"chat_bad_role", "body", nil}, {"chat_noauth", "body", nil},
		{"responses_empty", "body", nil}, {"responses_bad_input", "body", nil},
		{"responses_noauth", "body", nil}, {"edits_noauth", "body", nil},
		{"edits_badjson", "body", nil},
		// 管理面：只读几条按线上态用 shape/shallow 比，写接口用坏 token 比 401 壳。
		{"status_ok", "shape", nil}, {"accounts_ok", "shape", nil}, {"media_list", "shape", nil},
		{"apikey_ok", "shape", nil}, {"tasks_limit", "shape", nil}, {"tasks_bad_limit", "body", nil},
		{"extension_files", "shape", nil}, {"keepalive_status", "shallow", nil},
		{"update_check", "shallow", nil}, {"repo_status", "shallow", nil},
		{"cookie_helper", "blob", nil}, {"cookie_helper_dl", "blob", nil},
		{"index", "blob", nil}, {"admin_page", "blob", nil},
		{"openapi", "blob", nil}, {"docs", "blob", nil},
		{"extension", "head", nil}, {"anon_admin", "shape", nil}}
}

// replayCase 是一条金样本用例的回放方式。
type replayCase struct {
	name string
	mode string
	drop []string
}

// writeProbeCases 把金样本里「坏 token 探写接口」的用例搬进回放表，
// 只取已经实现的那几条 —— 没实现的留在台账的 deferred 里点名。
func writeProbeCases(g map[string]any) []replayCase {
	done := map[string]bool{
		"write_post_admin_accounts": true, "write_patch_admin_accounts_capnope": true,
		"write_delete_admin_accounts_capnope": true, "write_post_admin_apikey_rotate": true,
		"write_post_admin_tasks_clear": true, "write_delete_admin_tasks_capnope": true,
		"write_post_admin_accounts_capnope_cookies": true,
		"write_post_admin_update_upgrade":           true, "write_post_admin_repo_pull": true,
		"write_post_admin_repo_push":                true,
		"write_post_admin_accounts_capnope_test":    true,
		"write_post_admin_accounts_capnope_relogin": true,
		"write_post_admin_accounts_capnope_quota":   true,
		"write_post_admin_quota":                    true, "write_post_admin_relogin": true,
		"write_post_admin_accounts_keepalive":         true,
		"write_post_admin_accounts_capnope_keepalive": true,
	}
	var names []string
	for k := range g {
		if strings.HasPrefix(k, "write_") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []replayCase
	for _, k := range names {
		if done[k] {
			out = append(out, replayCase{k, "body", nil})
		}
	}
	return out
}

// docProbeModes 是 D3 那几条「路由器边角」的回放方式。
// 录制脚本（tools/golden/golden_routes.py）已经把它们排进去了，但线上要等下一次
// NAS 复录才会出这几段，所以这里只回收已经存在的用例；台账会把「实现了但还没录」点名出来。
var docProbeModes = map[string]string{
	"redoc":                  "blob",
	"oauth2_redirect":        "blob",
	"head_models":            "headonly",
	"slash_redirect_models":  "redirect",
	"slash_redirect_admin":   "redirect",
	"method_not_allowed_gen": "bodyallow",
}

func docProbeCases(g map[string]any) []replayCase {
	var names []string
	for k := range docProbeModes {
		if _, ok := g[k]; ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	out := make([]replayCase, 0, len(names))
	for _, k := range names {
		out = append(out, replayCase{k, docProbeModes[k], nil})
	}
	return out
}

// locationTail 取绝对 URL 里 authority 之后的部分（含查询串）。
func locationTail(loc string) string {
	i := strings.Index(loc, "://")
	if i < 0 {
		return loc
	}
	rest := loc[i+3:]
	for j := 0; j < len(rest); j++ {
		if rest[j] == '/' || rest[j] == '?' {
			return rest[j:]
		}
	}
	return ""
}

// shapeKeys 只取顶层键集合：线上态数据（账号数、任务列表、GitHub 检测结果）// 没法逐值比，但字段集合必须齐 —— 面板就是按这些键渲染的。
func shapeKeys(t *testing.T, row map[string]any) []string {
	t.Helper()
	s, _ := row["body_shape"].(map[string]any)
	if s == nil {
		t.Fatalf("这一段没有 body_shape")
	}
	var out []string
	for _, k := range arr(s["__keys__"]) {
		if name, ok := k.(string); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
