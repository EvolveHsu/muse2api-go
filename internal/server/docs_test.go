package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"muse2api-go/internal/webui"
)

// pendingDocPaths 是「代码已经对齐 1.5.4、内嵌的 openapi.json 还是 1.5.3 录的」那几条。
// 38465 升到 1.5.4 之后重录一次文档，这张表就该清空 —— 测试会检查它是否还有残留，
// 不允许它悄悄长成一个逃生口。
var pendingDocPaths = map[string]bool{
	"POST /admin/media/delete":   true,
	"DELETE /admin/media/{name}": true,
}

// docsAutoRoutes 是 FastAPI 装配时白挂出来的四条：它们不出现在自己生成的 openapi.json 里
// （include_in_schema=False），所以路由表对账要把它们单独列出来。
var docsAutoRoutes = map[string]bool{
	"GET /openapi.json":      true,
	"GET /docs":              true,
	"GET /redoc":             true,
	"GET " + oauth2RedirPath: true,
}

// TestRouteTableMatchesOpenAPIDoc 用线上生成的 openapi.json 反查 Go 的路由表：
// 文档里每一条 path×method 都得有对应路由，Go 里多出来的也只能是那四条自动路由。
// 这条测试同时是内嵌快照的保鲜机制 —— 以后加路由忘了同步文档，就会在这里红。
func TestRouteTableMatchesOpenAPIDoc(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi.python.json")
	if err != nil {
		t.Skip("缺 openapi.python.json:", err)
	}
	recorded, embedded := sha256.Sum256(raw), sha256.Sum256(webui.OpenAPIDoc())
	if hex.EncodeToString(recorded[:]) != hex.EncodeToString(embedded[:]) {
		t.Fatal("内嵌的 openapi.json 与线上录制不是同一串字节（行尾被规范化了？）")
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	mine := map[string]bool{}
	for _, rt := range a.routes {
		mine[rt.method+" "+rt.path] = true
	}
	var missing []string
	for p, ms := range doc.Paths {
		for m := range ms {
			key := strings.ToUpper(m) + " " + p
			if !mine[key] {
				missing = append(missing, key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("线上文档里有 %d 条路由 Go 没注册：%v", len(missing), missing)
	}
	var extra []string
	for k := range mine {
		if !docsAutoRoutes[k] && !pendingDocPaths[k] && !inDoc(doc.Paths, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("Go 多出线上文档之外的路由：%v（若确实新增，请同步 docsAutoRoutes 并重录文档）", extra)
	}
	for k := range pendingDocPaths {
		if inDoc(doc.Paths, k) {
			t.Errorf("%s 已经出现在文档里了，把 pendingDocPaths 里的这条删掉", k)
		}
	}
	if len(mine) != countDoc(doc.Paths)+len(docsAutoRoutes)+len(pendingDocPaths) {
		t.Errorf("路由条数对不上：go=%d 文档=%d+自动%d+待重录%d",
			len(mine), countDoc(doc.Paths), len(docsAutoRoutes), len(pendingDocPaths))
	}
}

func inDoc(paths map[string]map[string]json.RawMessage, key string) bool {
	sp := strings.SplitN(key, " ", 2)
	ms, ok := paths[sp[1]]
	if !ok {
		return false
	}
	_, ok = ms[strings.ToLower(sp[0])]
	return ok
}

func countDoc(paths map[string]map[string]json.RawMessage) int {
	n := 0
	for _, ms := range paths {
		n += len(ms)
	}
	return n
}

// TestDocsPages 钉住四条文档路由的状态码与 content-type。
// /openapi.json 与 /docs 已由线上金样本按字节比对（contract_test 的 blob 用例）；
// /redoc 与 /docs/oauth2-redirect 是同源转写，线上还没录过它们的体，
// 这里只保证结构不出格，等 tools/golden/golden_routes.py 新加的两条补录上来再按 sha 钉。
func TestDocsPages(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	cases := []struct {
		path string
		ct   string
		want []string
	}{
		{"/openapi.json", "application/json", []string{`"title":"muse2api"`, `"/v1/chat/completions"`}},
		{"/docs", "text/html; charset=utf-8", []string{"swagger-ui-bundle.js", "url: '/openapi.json'"}},
		{"/redoc", "text/html; charset=utf-8", []string{"redoc.standalone.js", `spec-url="/openapi.json"`}},
		{oauth2RedirPath, "text/html; charset=utf-8", []string{"Swagger UI: OAuth2 Redirect", "swaggerUIRedirectOauth2"}},
	}
	for _, c := range cases {
		rec := get(a, c.path, "")
		if rec.Code != 200 {
			t.Fatalf("%s 状态码 go=%d", c.path, rec.Code)
		}
		if ct := rec.Header().Get("content-type"); ct != c.ct {
			t.Errorf("%s content-type go=%q want=%q", c.path, ct, c.ct)
		}
		body := rec.Body.String()
		for _, s := range c.want {
			if !strings.Contains(body, s) {
				t.Errorf("%s 里应含 %q", c.path, s)
			}
		}
	}
}

// TestRawBodyCarriesContentLength：Starlette 的 Response 带正文就一定写 content-length。
// 不显式设的话 Go 在正文超过 2KB 后自动转 chunked —— 81020 字节的 admin.html 正好撞在这上面，
// 而金样本比对把头部的 content-length 列进了 DROP_HEADERS，所以这条只能在单测里钉。
func TestRawBodyCarriesContentLength(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	for _, c := range []struct {
		path string
		want string
	}{
		{"/docs", "1007"},
		{"/redoc", "889"},
		{"/openapi.json", "31219"},
		{"/admin", "81020"},
		{"/admin/cookie-helper", strconv.Itoa(len(webui.CookieHelper()))},
	} {
		rec := get(a, c.path, "")
		if got := rec.Header().Get("content-length"); got != c.want {
			t.Errorf("%s content-length go=%q want=%q（不设就会掉进 chunked）", c.path, got, c.want)
		}
		if rec.Body.Len() != mustAtoi(c.want) {
			t.Errorf("%s 正文长度 %d 与 content-length %s 不符", c.path, rec.Body.Len(), c.want)
		}
	}
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// TestHeadIsNotMatched：Starlette 的 Route 会给 GET 自动补 HEAD，**FastAPI 的 APIRoute 不补**。
// 线上 `HEAD /v1/models` 实测 405 + `allow: GET`（金样本 head_models 钉着）。
// 这条是「读 starlette 源码推断」被「真机 oracle」纠正的现场记录，别改回 200。
func TestHeadIsNotMatched(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	for _, p := range []string{"/healthz", "/v1/models", "/docs", "/openapi.json", "/admin/status"} {
		rec := doMethod(a, http.MethodHead, p, "cap-key")
		if rec.Code != 405 {
			t.Errorf("HEAD %s go=%d，线上是 405", p, rec.Code)
		}
		if got := rec.Body.String(); got != `{"detail":"Method Not Allowed"}` {
			t.Errorf("HEAD %s body=%s", p, got)
		}
	}
}

// TestMethodNotAllowedAllow：405 出自 APIRoute.handle 里带 headers 的 HTTPException，
// 所以除了 {"detail":...} 还要有 Allow；404 没有。
func TestMethodNotAllowedAllow(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	for _, c := range []struct{ method, path, allow string }{
		{"POST", "/v1/models", "GET"},
		{"HEAD", "/v1/models", "GET"},
		{"GET", "/v1/images/generations", "POST"},
		{"DELETE", "/docs", "GET"},
	} {
		rec := doMethod(a, c.method, c.path, "cap-key")
		if rec.Code != 405 {
			t.Fatalf("%s %s go=%d，应 405", c.method, c.path, rec.Code)
		}
		if got := rec.Header().Get("allow"); got != c.allow {
			t.Errorf("%s %s allow go=%q want=%q", c.method, c.path, got, c.allow)
		}
		if body := rec.Body.String(); body != `{"detail":"Method Not Allowed"}` {
			t.Errorf("%s %s body=%s", c.method, c.path, body)
		}
	}
	if rec := doMethod(a, "GET", "/nope", ""); rec.Header().Get("allow") != "" {
		t.Errorf("404 不该带 allow，go=%q", rec.Header().Get("allow"))
	}
}

// TestRedirectSlashes：路由表整条没沾上时，Starlette 会补/削尾斜杠再试一次，
// 成功就回 307 + 绝对 Location（查询串保留）。这一步发生在 405 之后。
func TestRedirectSlashes(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	cases := []struct {
		method, target, want string
	}{
		{"GET", "/v1/models/", "http://example.com/v1/models"},
		{"GET", "/admin/", "http://example.com/admin"},
		{"GET", "/docs/", "http://example.com/docs"},
		{"GET", "/healthz/", "http://example.com/healthz"},
		{"GET", "/v1/models//", "http://example.com/v1/models"},
	}
	for _, c := range cases {
		rec := doMethod(a, c.method, c.target, "cap-key")
		if rec.Code != 307 {
			t.Fatalf("%s %s go=%d，应 307（body=%s）", c.method, c.target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("location"); got != c.want {
			t.Errorf("%s %s location go=%q want=%q", c.method, c.target, got, c.want)
		}
	}
	// 查询串要跟着走。
	rec := doMethod(a, "GET", "/v1/models/?limit=1", "cap-key")
	if got := rec.Header().Get("location"); got != "http://example.com/v1/models?limit=1" {
		t.Errorf("带查询串的 307 location=%q", got)
	}
	// X-Forwarded-Proto 决定 Location 的 scheme（uvicorn 的 ProxyHeadersMiddleware 默认开着）。
	req := httptest.NewRequest("GET", "http://nas.example/v1/models/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	recW := httptest.NewRecorder()
	a.ServeHTTP(recW, req)
	if got := recW.Header().Get("location"); got != "https://nas.example/v1/models" {
		t.Errorf("转发后的 307 location=%q", got)
	}
	// 路径本身有偏匹配（方法不对）时先 405，不做斜杠重定向。
	if rec := doMethod(a, "POST", "/v1/models", "cap-key"); rec.Code != 405 {
		t.Errorf("POST /v1/models go=%d，应 405", rec.Code)
	}
	// 谁都不沾的路径仍然 404，不会误重定向。
	if rec := doMethod(a, "GET", "/v1/nope/", "cap-key"); rec.Code != 404 {
		t.Errorf("GET /v1/nope/ go=%d，应 404", rec.Code)
	}
}

// TestLocationTail 是金样本 redirect 回放要用的小工具：Location 是绝对地址，
// 主机名跟着请求走，所以比对时只取 authority 之后那截。
func TestLocationTail(t *testing.T) {
	for _, c := range [][2]string{
		{"http://nas:38465/v1/models?x=1", "/v1/models?x=1"},
		{"http://nas:38465", ""},
		{"http://nas:38465/", "/"},
		{"/v1/models", "/v1/models"},
	} {
		if got := locationTail(c[0]); got != c[1] {
			t.Errorf("locationTail(%q) go=%q want=%q", c[0], got, c[1])
		}
	}
}

func get(a *App, path, key string) *httptest.ResponseRecorder {
	return doMethod(a, "GET", path, key)
}

func doMethod(a *App, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com"+path, nil)
	// httptest.NewRequest 把整串当 target 解析，查询串与路径都会落到 URL 上。
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}
