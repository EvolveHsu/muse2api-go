// Package server 对应 app.py：路由、鉴权、CORS、FastAPI 的两套错误壳与响应写法。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"muse2api-go/internal/config"
	"muse2api-go/internal/engine"
	"muse2api-go/internal/pyval"
	"muse2api-go/internal/store"
)

// AppError 对应 HTTPException(status_code, detail, headers)。
type AppError struct {
	Status  int
	Detail  any
	Headers map[string]string
}

func (e *AppError) Error() string {
	s, ok := e.Detail.(string)
	if !ok {
		return http.StatusText(e.Status)
	}
	return s
}

func httpError(status int, detail any) *AppError {
	return &AppError{Status: status, Detail: detail}
}

// errType 对应 _err_type()。
func errType(status int) string {
	switch {
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	}
	return "invalid_request_error"
}

type reqCtx struct {
	W    http.ResponseWriter
	R    *http.Request
	Args map[string]string
}

type handlerFunc func(c *reqCtx) (any, error)

type segment struct {
	lit   string
	param bool
}

type route struct {
	method string
	segs   []segment
	auth   bool
	h      handlerFunc
	path   string
}

// App 持有 app.py 的模块级状态：store/engine 单例、两把锁、路由表。
type App struct {
	CFG    *config.Config
	Store  *store.Store
	Engine *engine.Engine
	// browser 对应 GEN_LOCK（全局串行），imgTaskLock 对应 IMAGE_TASK_LOCK（只管入队那一段）。
	browser     *browserLock
	imgTaskLock *sync.Mutex

	routes []*route

	// mu 护着运行时可改的配置（目前只有 API Key 轮换）；
	// updMu/kaMu 对应 _UPDATE_CACHE 与保活状态那两份模块级可变对象。
	mu       sync.Mutex
	updMu    sync.Mutex
	updCache updateCache
	kaMu     sync.Mutex
	kaState  keepaliveState

	// now 是墙钟（落盘时间戳），mono 是单调钟（deadline）。Python 也是这么混用的，
	// 不「顺手统一」：前者会被系统校时拉动，后者不会 —— Go 的 time.Time 自带单调读数，
	// 只要两端都取自 time.Now，Sub/Before 走的就是单调钟。
	now  func() time.Time
	mono func() time.Time

	// accessOut 是 access log 的去处，默认 stderr（与 uvicorn 一致）；测试注入 buffer。
	accessOut io.Writer
}

func NewApp(cfg *config.Config, st *store.Store, eng *engine.Engine) *App {
	a := &App{
		CFG: cfg, Store: st, Engine: eng,
		browser:     newBrowserLock(),
		imgTaskLock: &sync.Mutex{},
		now:         time.Now,
		mono:        time.Now,
	}
	a.registerRoutes()
	return a
}

// --- 路由表 ---

// pat 支持 {name} 形式的单段通配，语义与 Starlette 的 [^/]+ 一致：
// 一个参数只吃一段，段里出现（解码后的）'/' 就算整条路径不匹配 → 404，
// 这跟 Python 对 /v1/media/..%2Faccounts.json 的行为逐字对齐。
//
// 不 Trim 斜杠：Starlette 编译出来的是 ^/admin$ 这种整串匹配，
// 所以 /admin/ 是不匹配的（它走 redirect_slashes 变 307），不能顺手当 /admin 用。
func parsePattern(p string) []segment {
	var out []segment
	for _, s := range strings.Split(p, "/") {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			out = append(out, segment{lit: s[1 : len(s)-1], param: true})
			continue
		}
		out = append(out, segment{lit: s})
	}
	return out
}

func (a *App) add(method, pattern string, auth bool, h handlerFunc) {
	a.routes = append(a.routes, &route{method: method, path: pattern,
		segs: parsePattern(pattern), auth: auth, h: h})
}

func matchPath(segs []segment, path string) (map[string]string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != len(segs) {
		return nil, false
	}
	args := map[string]string{}
	for i, s := range segs {
		if s.param {
			if parts[i] == "" {
				return nil, false
			}
			args[s.lit] = parts[i]
			continue
		}
		if parts[i] != s.lit {
			return nil, false
		}
	}
	return args, true
}

// ServeHTTP 复刻 FastAPI 的分派顺序：先 CORS，再路由匹配（404/405 由路由器给出，
// 走的是 Starlette 默认处理器 —— 未匹配的 /v1 路径**不会**套上 OpenAI 错误壳，
// 金样本 media_enc_slash_only / v1_unmatched 就是把这条钉住），最后才轮到鉴权依赖。
//
// 匹配顺序照 fastapi/routing.py::Router.app：整匹配 → 首个「路径对但方法不对」的偏匹配（405 + Allow）
// → redirect_slashes（补/去尾斜杠后能匹配就 307）→ 404。
// llhttpMethods 是线上解析器认的那 35 个方法名。名单来自部署中的 llhttp enum，
// 再用 53 条实测钉住（testdata/method_table.python.json）：enum 里 PRI 往后的那批
// RTSP/SIP 方法（PRI DESCRIBE ANNOUNCE SETUP PLAY PAUSE TEARDOWN GET_PARAMETER
// SET_PARAMETER REDIRECT RECORD FLUSH）实际会被拒，所以不在表内；
// 表外的写法一律 400 —— 包括任何小写或混合大小写的 get / Get / PoSt。
var llhttpMethods = map[string]bool{
	"DELETE": true, "GET": true, "HEAD": true, "POST": true, "PUT": true,
	"CONNECT": true, "OPTIONS": true, "TRACE": true, "COPY": true, "LOCK": true,
	"MKCOL": true, "MOVE": true, "PROPFIND": true, "PROPPATCH": true, "SEARCH": true,
	"UNLOCK": true, "BIND": true, "REBIND": true, "UNBIND": true, "ACL": true,
	"REPORT": true, "MKACTIVITY": true, "CHECKOUT": true, "MERGE": true,
	"M-SEARCH": true, "NOTIFY": true, "SUBSCRIBE": true, "UNSUBSCRIBE": true,
	"PATCH": true, "PURGE": true, "MKCALENDAR": true, "LINK": true, "UNLINK": true,
	"SOURCE": true,
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DEVIATION(#20): 协议层的方法表先拦一道：线上 uvicorn 用的解析器是 httptools（llhttp），不是 h11，
	// 表外的方法连 ASGI 都进不去 —— 直接 400 纯文本，既不过 CORS 中间件，也不打 access log。
	// Go 的 net/http 不设这张表，不拦就会把 `post /v1/images/generations` 这种请求当 POST 受理，
	// 于是客户端少打一个大小写都算不上的错，我们却替它花掉一次额度。
	if !llhttpMethods[r.Method] {
		writePlain(w, http.StatusBadRequest, "Invalid HTTP request received.")
		return
	}
	rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	if a.accessEnabled() {
		defer func() { a.logAccess(rec, r) }()
	}
	applyCORS(rec, r)
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		preflight(rec, r)
		return
	}
	method := strings.ToUpper(r.Method)
	var partial *route
	for _, rt := range a.routes {
		args, ok := matchPath(rt.segs, r.URL.Path)
		if !ok {
			continue
		}
		if !methodAllowed(rt.method, method) {
			if partial == nil {
				partial = rt // Starlette 也只记第一个偏匹配
			}
			continue
		}
		a.dispatch(rec, r, rt, args)
		return
	}
	if partial != nil {
		a.writeRouterError(rec, http.StatusMethodNotAllowed, "Method Not Allowed",
			map[string]string{"allow": allowedMethods(partial.method)})
		return
	}
	if loc, ok := a.redirectSlash(r); ok {
		// RedirectResponse：307，只有 location 头，正文空。
		rec.Header().Set("location", loc)
		rec.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	a.writeRouterError(rec, http.StatusNotFound, "Not Found", nil)
}

// statusWriter 只为 access log 记下最终状态码：Go 的 ResponseWriter 不暴露它
// —— 没显式 WriteHeader 时状态是 net/http 在第一次写时补的 200，所以我们默认 200、
// 由第一次 WriteHeader 覆盖（后面的重复调用不改，客户端看到的就是第一个）。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.wrote, s.status = true, code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Flush 必须透传：SSE 那两条路由是 `c.W.(http.Flusher)` 拿 flusher 的，
// 包一层之后若不实现，断言失败会静默退化成「攒完才发」，流式就没有流式了。
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) accessEnabled() bool { return a.CFG == nil || a.CFG.AccessLog }

func (a *App) logAccess(rec *statusWriter, r *http.Request) {
	out := a.accessOut
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintln(out, accessLine(rec, r))
}

// accessLine 复刻 uvicorn 的 access log 行：`INFO:` 补足到 10 个字符，然后
// `client:port - "METHOD TARGET PROTO" 状态码 状态短语`。
//
// TARGET 不是裸的请求行 —— 线上实测 uvicorn 打的是 quote(unquote(target))：`%2F` 还原成 `/`、
// `%2e%2e` 还原成 `..`，而 `%20`、`%252F`、`/./`、`//` 原样保留（金样本 28 行里
// a17/w1/w7 三条就是这个差异，其中 w1/w7 是裸 socket 发出去的，排除了客户端自己改写的解释）。
// 查询串按原始字节接在后面，不参与这轮转义。
//
// client 用 host 与 port 直接拼（uvicorn 是 `f"{client[0]}:{client[1]}"`），所以 IPv6
// 的地址没有方括号 —— 与 Go 的 RemoteAddr 写法不同，这里按 Python 的来。
func accessLine(rec *statusWriter, r *http.Request) string {
	target := pyQuote(r.URL.Path, "/")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host, port = r.RemoteAddr, "0"
	}
	return fmt.Sprintf("%-10s%s:%s - \"%s %s %s\" %d %s",
		"INFO:", host, port, r.Method, target, r.Proto, rec.status, http.StatusText(rec.status))
}

// methodAllowed：FastAPI 的 APIRoute 只把自己声明的那一个方法放进 methods，
// **不像** Starlette 的 Route 那样给 GET 自动补 HEAD —— 线上 `HEAD /v1/models` 实测是
// 405 + `allow: GET`（金样本 head_models 钉着），所以这里就是严格相等。
func methodAllowed(routeMethod, method string) bool { return routeMethod == method }

// allowedMethods 对应 405 里的 `", ".join(methods)`。
func allowedMethods(routeMethod string) string { return routeMethod }

// redirectSlash 是 Starlette 的 redirect_slashes：只有「整条路由表都没沾上」时才会走到这里，
// Location 用 scope 里的 path（已解码）+ query 拼绝对 URL，再按 Python quote 的那串 safe 集重新转义。
func (a *App) redirectSlash(r *http.Request) (string, bool) {
	if r.URL.Path == "/" {
		return "", false
	}
	target := r.URL.Path
	if strings.HasSuffix(target, "/") {
		target = strings.TrimRight(target, "/")
	} else {
		target += "/"
	}
	matched := false
	for _, rt := range a.routes {
		if _, ok := matchPath(rt.segs, target); ok {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	u := requestScheme(r) + "://" + r.Host + target
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return pyQuote(u, `:/%#?=@[]!$&'()*+,;`), true
}

func requestScheme(r *http.Request) string {
	// uvicorn 默认开着 ProxyHeadersMiddleware，但它只认 X-Forwarded-Proto 的第一个值、
	// 且只接受 http/https —— 别的写法一律当没这头。
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		first := strings.ToLower(strings.TrimSpace(strings.Split(v, ",")[0]))
		if first == "https" || first == "http" {
			return first
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// pyQuote 对应 urllib.parse.quote(s, safe=...)：字母数字与 _.~- 永远不编码，
// safe 里的字符原样，其余按 UTF-8 逐字节 %XX（大写十六进制）。
func pyQuote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_' || c == '.' || c == '-' || c == '~', strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// writeRouterError：路由自身的 404/405 出自 Starlette 的默认异常处理，
// 它**不看路径前缀** —— 即便请求的是 /v1/xxx 这种没注册上的地址，回的还是 {"detail":...}
// 而不是 OpenAI 错误壳。金样本 media_enc_slash_only / v1_unmatched 就是把这条钉住的
// （handler 里主动 raise 的 404 才会套壳，见 media_missing）。
// 405 由 APIRoute.handle 里 `raise HTTPException(405, headers={"Allow": ...})` 抛出，
// 所以它比 404 多一个 Allow 头。
func (a *App) writeRouterError(w http.ResponseWriter, status int, detail string, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	jsonOut(w, status, map[string]any{"detail": detail})
}

func (a *App) dispatch(w http.ResponseWriter, r *http.Request, rt *route, args map[string]string) {
	ctx := &reqCtx{W: w, R: r, Args: args}
	if rt.auth {
		if err := a.auth(r); err != nil {
			ae := err.(*AppError)
			a.writeError(w, r, ae.Status, ae.Detail, ae.Headers)
			return
		}
	}
	res, err := rt.h(ctx)
	if err != nil {
		if ae, ok := err.(*AppError); ok {
			a.writeError(w, r, ae.Status, ae.Detail, ae.Headers)
			return
		}
		// 未预期异常：Starlette 的 ServerErrorMiddleware 只回 500 "Internal Server Error"，
		// 金样本 edits_badjson 钉住了它。Go 这边同样不把内部错误文案漏给客户端。
		writePlain(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	switch v := res.(type) {
	case nil:
		jsonOut(w, http.StatusOK, map[string]any{})
	case *rawResponse:
		v.write(ctx)
	default:
		jsonOut(w, http.StatusOK, v)
	}
}

// --- 鉴权（app.py: auth()） ---

// apiKey 读当前密钥。写它的有两处（启动时自动生成、面板轮换 Key），都在 a.mu 下；
// 读这边也必须上锁 —— Python 里 `CFG.api_key` 是 GIL 保护的单次属性读，
// Go 里对一个 string（指针+长度两个字）无锁读写就是数据竞争，轮换瞬间可能读到撕裂值。
func (a *App) apiKey() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CFG.APIKey
}

func (a *App) auth(r *http.Request) error {
	key := a.apiKey()
	if key == "" {
		return nil
	}
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return httpError(401, "缺少 Authorization: Bearer <key>")
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	if token == "" || token != key {
		return httpError(401, "API key 无效")
	}
	return nil
}

// --- 响应写法（对齐 FastAPI 的 JSONResponse / PlainTextResponse） ---

// jsonOut：FastAPI 用 json.dumps(..., ensure_ascii=False, separators=(",", ":"))，
// 即紧凑分隔符、非 ASCII 原样输出、**不**转义 < > &，且结尾没有换行。
func jsonOut(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		writePlain(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeError 对应 app.py 的 _http_exc：/v1/* 换成 OpenAI 的错误壳，其余保持 {"detail":...}，
// 因为管理面板是按 detail 取信息的。
func (a *App) writeError(w http.ResponseWriter, r *http.Request, status int, detail any, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		msg, ok := detail.(string)
		if !ok {
			b, _ := json.Marshal(detail)
			msg = string(b)
		}
		// 键序必须是 message/type/param/code：FastAPI 的 json.dumps 保插入序，
		// 用 map 会被 encoding/json 按字母排成 code/message/param/type（跨服务实测抓到）。
		jsonOut(w, status, pyval.Obj{{"error", pyval.Obj{
			{"message", msg}, {"type", errType(status)}, {"param", nil}, {"code", status}}}})
		return
	}
	jsonOut(w, status, pyval.Obj{{"detail", detail}})
}

// --- CORS（app.py 的 add_middleware：allow_origins=["*"] 等） ---

const corsMethods = "GET, POST, PATCH, DELETE, OPTIONS"

func applyCORS(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("vary", "Origin")
	if r.Header.Get("Origin") == "" {
		return
	}
	h.Set("access-control-allow-origin", "*")
	h.Set("access-control-expose-headers", "*")
}

func preflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if req := r.Header.Get("Access-Control-Request-Method"); req != "" {
		h.Set("access-control-allow-methods", corsMethods)
	}
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		h.Set("access-control-allow-headers", req)
	}
	// Starlette 在这条路上会把四个请求头名一起写进单个 Vary，金样本逐字记着。
	h.Set("vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers, Access-Control-Request-Private-Network")
	h.Set("access-control-allow-origin", "*")
	h.Set("access-control-max-age", "600")
	writePlain(w, http.StatusOK, "OK")
}
