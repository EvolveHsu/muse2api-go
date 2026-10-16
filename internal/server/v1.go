package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"muse2api-go/internal/engine"
	"muse2api-go/internal/pyval"
)

// v1.go 对应 app.py 的「推理面」：生图、生视频、对话、Responses。
// 每条路由的错误码映射照抄：MuseAuthError→401、MuseGenerationError→502，
// 其余异常一路穿到 500（与 Starlette 的 ServerErrorMiddleware 一致）。

func (a *App) registerV1() {
	a.add("POST", "/v1/images/generations", true, a.imagesGenerations)
	a.add("POST", "/v1/images/edits", true, a.imagesEdits)
	a.add("POST", "/v1/images/tasks", true, a.imagesTasks)
	a.add("GET", "/v1/images/tasks/{task_id}", true, a.getImageTask)
	a.add("POST", "/v1/videos", true, a.createVideo)
	a.add("POST", "/v1/videos/generations", true, a.createVideo)
	a.add("GET", "/v1/videos/{task_id}", true, a.getVideo)
	a.add("GET", "/v1/videos/generations/{task_id}", true, a.getVideo)
	a.add("POST", "/v1/chat/completions", true, a.chatCompletions)
	a.add("POST", "/v1/responses", true, a.responsesAPI)
}

// resolveRef 对应三条路由里重复出现的「参考图取哪个字段」。
func resolveRef(reference *string, image any, images []any) string {
	if reference != nil && *reference != "" {
		return *reference
	}
	if v := refFromImage(image); v != "" {
		return v
	}
	return refFromImages(images)
}

func (a *App) imagesGenerations(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	req, errs := parseImageRequest(body)
	if len(errs) > 0 {
		return nil, a.validationFailed(c.R, errs)
	}
	ref := resolveRef(req.Reference, req.Image, req.Images)
	prompt := buildImagePrompt(req)
	timeout := int(pyNumOr(req.Timeout, int64(a.CFG.ImageTimeout)))
	if req.Async {
		return a.queueImage(c, req, prompt, ref, nil)
	}
	res, _, err := a.runGeneration(prompt, "image", timeout, "", nil, ref, c.R.Context().Done(), nil)
	if err != nil {
		return nil, mapGenErr(err)
	}
	return a.imageResponse(req, res), nil
}

// mapGenErr 对应路由里的两段 except：认证失败 401，生成失败 502。
func mapGenErr(err error) error {
	var ae *engine.ErrAuth
	var ge *engine.ErrGeneration
	switch {
	case errors.As(err, &ae):
		return httpError(401, ae.Error())
	case errors.As(err, &ge):
		return httpError(502, ge.Error())
	}
	return err
}

// errModelRejected 对应 handler 里 `ImageRequest(**字段)` 抛出的 pydantic ValidationError。
// 它不在 FastAPI 的校验层里，所以没有 422 —— 是未捕获异常，客户端只拿到 500 纯文本。
var errModelRejected = errors.New("ImageRequest 构造抛 ValidationError")

// errMaxType 对应 `_run_generation` 第一行的 `max(1, timeout)`：
// 传进去的是那个**局部原始值**，字符串能过 pydantic 却过不了 max → TypeError → 500。
var errMaxType = errors.New("max(1, timeout) TypeError")

// imagesEdits 对应 images_edits()。这个路由的签名是 `request: Request`，FastAPI 不做 body 校验，
// handler 自己解析完再 `req_obj = ImageRequest(...)` —— 于是校验失败的形态是 500 而不是 422，
// 并且一律发生在进 _run_generation 之前：浏览器不会起、额度不会花。
// 金样本 edits_model_00..16 逐字段钉住了这件事。
func (a *App) imagesEdits(c *reqCtx) (any, error) {
	ct := strings.ToLower(c.R.Header.Get("content-type"))
	var asyncRaw, promptRaw, modelRaw, sizeRaw, arRaw, formatRaw, timeoutRaw, refRaw any
	if strings.Contains(ct, "multipart/form-data") {
		if err := c.R.ParseMultipartForm(32 << 20); err != nil {
			return nil, err
		}
		f := c.R.MultipartForm.Value
		asyncRaw = formGet(f, "async", false)
		promptRaw = pyOrAny(formGet(f, "prompt", nil), "")
		modelRaw = pyOrAny(formGet(f, "model", nil), "muse-image")
		sizeRaw = formGet(f, "size", nil)
		arRaw = formGet(f, "aspect_ratio", nil)
		formatRaw = pyOrAny(formGet(f, "response_format", nil), "url")
		timeoutRaw = formTimeout(f)
		refRaw = formImageRef(c.R, f)
	} else {
		// 坏 JSON 与非对象 body 都从这里出去（500），edits_badjson 钉着。
		body, err := manualJSONBody(c.R)
		if err != nil {
			return nil, err
		}
		asyncRaw = jsonGet(body, "async", false)
		promptRaw = pyOrAny(body["prompt"], "")
		modelRaw = pyOrAny(body["model"], "muse-image")
		sizeRaw = body["size"]
		arRaw = body["aspect_ratio"]
		formatRaw = pyOrAny(body["response_format"], "url")
		timeoutRaw = body["timeout"]
		refRaw = editsJSONRef(body)
	}
	// if not prompt: prompt = "参考此图片并进行生图创作"
	if !truthy(promptRaw) {
		promptRaw = "参考此图片并进行生图创作"
	}

	// Python 在这里构造 ImageRequest(prompt=…, model=…, size=…, aspect_ratio=…,
	// response_format=…, timeout=…, reference_image=…, **{"async": …}) —— 只有这八个字段，
	// 所以 n / extra / images 传坏了也不影响这条路由，字段表里就一个都不能多。
	req, errs := parseImageRequest(map[string]any{
		"prompt": promptRaw, "model": modelRaw, "size": sizeRaw, "aspect_ratio": arRaw,
		"response_format": formatRaw, "timeout": timeoutRaw,
		"reference_image": refRaw, "async": asyncRaw})
	if len(errs) > 0 {
		return nil, errModelRejected
	}
	full := buildImagePrompt(req)
	ref, _ := refRaw.(string) // 过了 reference_image: str | None，剩下的只可能是字符串或 nil
	if req.Async {
		return a.queueImage(c, req, full, ref, nil)
	}
	genTimeout, ok := genTimeoutOr(timeoutRaw, int64(a.CFG.ImageTimeout))
	if !ok {
		return nil, errMaxType
	}
	res, _, err := a.runGeneration(full, "image", int(genTimeout), "", nil, ref, c.R.Context().Done(), nil)
	if err != nil {
		return nil, mapGenErr(err)
	}
	return a.imageResponse(req, res), nil
}

// genTimeoutOr 对应 `timeout = body.get("timeout")` 之后的 `timeout or CFG.image_timeout`，
// 以及 `_run_generation` 第一行的 `max(1, timeout)`。只有数字能活着走完这两步：
// 字符串在 max 上 TypeError，None/0 落默认值。
func genTimeoutOr(raw any, def int64) (int64, bool) {
	if !truthy(raw) {
		return def, true
	}
	switch t := raw.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return int64(f), true
	case int64:
		return t, true
	}
	return 0, false
}

// formTimeout 对应 multipart 分支那三连：`if timeout_val: try int(timeout_val) except ValueError: pass`。
// 空串与非字符串字段都留在 None，转不出整数也留在 None（于是走默认超时）。
func formTimeout(f map[string][]string) any {
	if v, ok := f["timeout"]; !ok || len(v) == 0 || v[0] == "" {
		return nil
	} else if n, err := parseInt(v[0]); err == nil {
		return n
	}
	return nil
}

// formImageRef 对应 multipart 的 image 字段：有 read() 的上传件转成 data URL，
// 纯文本字段原样，其余形态都不认（ref_image_data 留在 None）。
func formImageRef(r *http.Request, f map[string][]string) any {
	if file, hdr, err := r.FormFile("image"); err == nil {
		defer file.Close()
		data, rerr := io.ReadAll(file)
		if rerr != nil {
			return nil
		}
		mimeT := hdr.Header.Get("Content-Type")
		if mimeT == "" {
			mimeT = "image/png"
		}
		return "data:" + mimeT + ";base64," + encodeB64(data)
	}
	if v, ok := f["image"]; ok && len(v) > 0 {
		return v[0]
	}
	return nil
}

// editsJSONRef 对应 JSON 分支那段四级兜底，逐层都是 Python 的 `or`，
// 所以值必须保持原始类型往下传 —— {"image":{"url":5}} 要带着 5 去撞 reference_image 的 str 校验。
func editsJSONRef(body map[string]any) any {
	var ref any
	switch v := body["image"].(type) {
	case map[string]any:
		ref = pyOrAny(v["url"], v["b64_json"])
	case string:
		ref = v
	}
	if l, ok := body["images"].([]any); ok && !truthy(ref) && len(l) > 0 {
		switch first := l[0].(type) {
		case map[string]any:
			ref = pyOrAny(first["image_url"], first["url"])
		case string:
			ref = first
		}
	}
	if !truthy(ref) {
		ref = pyOrAny(body["reference_image"], body["image_url"])
	}
	return ref
}

func (a *App) imagesTasks(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	req, errs := parseImageRequest(body)
	if len(errs) > 0 {
		return nil, a.validationFailed(c.R, errs)
	}
	ref := resolveRef(req.Reference, req.Image, req.Images)
	key := c.R.Header.Get("Idempotency-Key")
	var keyPtr *string
	if key != "" {
		keyPtr = &key
	}
	return a.queueImage(c, req, buildImagePrompt(req), ref, keyPtr)
}

func (a *App) getImageTask(c *reqCtx) (any, error) {
	task := a.Store.GetTask(c.Args["task_id"])
	if task == nil || task["kind"] != "image" {
		return nil, httpError(404, "image task 不存在")
	}
	out := cloneMap(task)
	if strOf(out["status"]) == "completed" {
		req := &ImageRequest{Prompt: strOf(out["prompt"]), Size: optStr(out["size"]),
			ResponseFormat: orDefaultStr(strOf(out["response_format"]), "url")}
		res := resultFromMap(out["result"])
		for _, p := range a.imageResponse(req, res) {
			out[p.Key] = p.Val
		}
	}
	return out, nil
}

func (a *App) createVideo(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	req, errs := parseVideoRequest(body)
	if len(errs) > 0 {
		return nil, a.validationFailed(c.R, errs)
	}
	ref := ""
	if req.Reference != nil {
		ref = *req.Reference
	}
	if ref == "" {
		ref = refFromImage(req.ImageURL)
	}
	if ref == "" {
		ref = refFromImage(req.Image)
	}
	prompt := buildVideoPrompt(req)
	timeout := int(pyNumOr(req.Timeout, int64(a.CFG.VideoTimeout)))
	task := a.Store.CreateTask("video", req.Prompt)
	tid := strOf(task["id"])
	a.Store.UpdateTask(tid, map[string]any{"api_prompt": prompt})
	a.Store.UpdateTask(tid, map[string]any{"progress": 10})

	go func() {
		t0 := a.now()
		onAcquired := func() {
			// 拿到浏览器槽位才开始计时/进 processing：排队等待不算任务耗时（上游 1.6.1）。
			t0 = a.now()
			a.Store.UpdateTask(tid, map[string]any{"status": "processing", "progress": 15})
		}
		res, accID, err := a.runGeneration(prompt, "video", timeout, "",
			func(p int) { a.Store.UpdateTask(tid, map[string]any{"progress": p}) }, ref, nil, onAcquired)
		if err != nil {
			a.Store.UpdateTask(tid, map[string]any{"status": "failed",
				"elapsed": round1(a.now().Sub(t0).Seconds()), "error": err.Error()})
			return
		}
		vurl := a.mediaURL(res.Filename)
		a.Store.UpdateTask(tid, map[string]any{
			"status": "completed", "progress": 100, "account": accID,
			"elapsed": round1(a.now().Sub(t0).Seconds()), "url": vurl,
			"video": pyval.Obj{{"url", vurl}},
			"result": pyval.Obj{{"url", vurl}, {"filename", res.Filename},
				{"bytes", res.Size}, {"kind", res.Kind}},
		})
	}()

	return pyval.Obj{
		{"id", tid}, {"task_id", tid}, {"object", "video.task"},
		{"status", "queued"}, {"progress", 10}, {"created_at", task["created_at"]},
	}, nil
}

// getVideo 对应 get_video()：把内部状态词翻译成 OpenAI 的，并按耗时推算进度。
func (a *App) getVideo(c *reqCtx) (any, error) {
	t := a.Store.GetTask(c.Args["task_id"])
	if t == nil {
		return nil, httpError(404, "task 不存在")
	}
	out := cloneMap(t)
	switch strOf(out["status"]) {
	case "succeeded", "success", "done":
		out["status"] = "completed"
	}
	switch strOf(out["status"]) {
	case "completed":
		out["progress"] = 100
	case "processing":
		elapsed := float64(a.now().Unix()) - numOr(out["created_at"])
		calc := 20 + elapsed*1.1
		if calc > 92 {
			calc = 92
		}
		if int(calc) > orDefaultNum(out["progress"], 0) {
			out["progress"] = int(calc)
		}
	}
	vurl := strOf(out["url"])
	if vurl == "" {
		if m, ok := out["result"].(map[string]any); ok {
			vurl = strOf(m["url"])
		}
	}
	if vurl != "" {
		out["url"] = vurl
		if _, has := out["video"]; !has {
			out["video"] = pyval.Obj{{"url", vurl}}
		}
	}
	return out, nil
}

// --- 对话 ---

func (a *App) chatCompletions(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	req, errs := parseChatRequest(body)
	if len(errs) > 0 {
		return nil, a.validationFailed(c.R, errs)
	}
	prompt := buildChatPrompt(req.Messages)
	if prompt == "" {
		prompt = strings.TrimSpace(deref(req.Prompt))
	}
	if prompt == "" {
		return nil, httpError(400, "messages 为空")
	}
	toolNote := ""
	if a.CFG.ToolProtocol {
		toolNote = buildToolsPrompt(req.Tools)
		if toolNote != "" {
			prompt = prompt + "\n\n" + toolNote
		}
	}
	model := resolveModel(optStr(req.Model), "muse-spark")
	timeout := int(pyNumOr(req.Timeout, int64(a.CFG.ChatTimeout)))
	acc := a.Store.PickAccount(a.Engine.CurrentAccountID(), true, false, "", nil, true)
	if acc == nil {
		return nil, httpError(400, "没有可用账号，请先在管理页面导入 cookie")
	}
	cid := "chatcmpl-" + uuidHex(24)
	created := a.now().Unix()
	accID := strOf(acc["id"])
	cookies := toStringMap(acc["cookies"])
	expires := cookieIntMap(acc["cookies_exp"])

	if req.Stream {
		return a.chatStreamSSE(c, cid, created, model, accID, cookies, expires,
			prompt, timeout, toolNote, req)
	}
	text, err := collectChat(a.safeChatStream(accID, cookies, expires, prompt, timeout, nil))
	if err != nil {
		return nil, mapGenErr(err)
	}
	usage := pyval.Obj{{"prompt_tokens", 0}, {"completion_tokens", 0}, {"total_tokens", 0}}
	if toolNote != "" {
		calls, rest := parseToolCalls(text)
		if len(calls) > 0 {
			content := any(rest)
			if rest == "" {
				content = nil
			}
			return pyval.Obj{
				{"id", cid}, {"object", "chat.completion"}, {"created", created},
				{"model", model},
				{"choices", []any{pyval.Obj{
					{"index", 0}, {"finish_reason", "tool_calls"},
					{"message", pyval.Obj{{"role", "assistant"}, {"content", content},
						{"tool_calls", calls}}},
				}}},
				{"usage", usage},
			}, nil
		}
	}
	return pyval.Obj{
		{"id", cid}, {"object", "chat.completion"}, {"created", created},
		{"model", model},
		{"choices", []any{pyval.Obj{
			{"index", 0}, {"finish_reason", "stop"},
			{"message", pyval.Obj{{"role", "assistant"}, {"content", text}}},
		}}},
		{"usage", usage},
	}, nil
}

func (a *App) chatStreamSSE(c *reqCtx, cid string, created int64, model, accID string,
	cookies map[string]string, expires map[string]int, prompt string, timeout int,
	toolNote string, req *ChatRequest) (any, error) {
	stop := make(chan struct{})
	go func() {
		<-c.R.Context().Done()
		close(stop)
	}()
	return &rawResponse{
		Status:  http.StatusOK,
		Headers: sseHeaderMap(),
		Stream: func(cc *reqCtx) error {
			return a.writeChatStream(cc, cid, created, model, accID, cookies, expires,
				prompt, timeout, toolNote, req, stop)
		},
	}, nil
}

// writeChatStream 对应 chat_completions 里的 sync_stream()：
// 首包立刻给 role: assistant 让客户端秒级拿到光标，之后按 _pace_text 的节奏吐字。
func (a *App) writeChatStream(c *reqCtx, cid string, created int64, model, accID string,
	cookies map[string]string, expires map[string]int, prompt string, timeout int,
	toolNote string, req *ChatRequest, stop <-chan struct{}) error {

	flusher, _ := c.W.(http.Flusher)
	emit := func(s string) error {
		if _, err := c.W.Write([]byte(s)); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	// 首包在生成开始之前就要发出去，所以先握手再进流。
	if err := emit(sse(chatChunk(cid, created, model, pyval.Obj{{"role", "assistant"}}, nil))); err != nil {
		return err
	}

	buf, mode := "", ""
	chunks := a.safeChatStream(accID, cookies, expires, prompt, timeout, stop)
	var streamErr error
	for it := range chunks {
		if it.Err != nil {
			streamErr = it.Err
			break
		}
		chunk := it.Text
		if toolNote == "" {
			if err := paceEmit(chunk, 2, 12*time.Millisecond, pieceFn(c, cid, created, model, emit)); err != nil {
				return err
			}
			continue
		}
		if mode == "text" {
			if err := paceEmit(chunk, 2, 12*time.Millisecond, pieceFn(c, cid, created, model, emit)); err != nil {
				return err
			}
			continue
		}
		buf += chunk
		if mode == "" {
			head := strings.TrimLeft(buf, " \t\n")
			if len([]rune(head)) >= 2 {
				if strings.HasPrefix(head, "{") || strings.HasPrefix(head, "[") ||
					strings.HasPrefix(head, "```") {
					mode = "maybe_tool"
				} else {
					mode = "text"
					if err := paceEmit(buf, 2, 12*time.Millisecond, pieceFn(c, cid, created, model, emit)); err != nil {
						return err
					}
					buf = ""
				}
			}
		}
	}
	if streamErr != nil {
		return emit(sse(sseErrChunk(streamErr, accID, a)))
	}
	if mode == "text" {
		if err := emit(sse(chatChunk(cid, created, model, pyval.Obj{}, "stop"))); err != nil {
			return err
		}
	} else {
		calls, rest := parseToolCalls(buf)
		if len(calls) > 0 {
			if rest != "" {
				if err := paceEmit(rest, 2, 12*time.Millisecond, pieceFn(c, cid, created, model, emit)); err != nil {
					return err
				}
			}
			for _, piece := range toolCallDeltas(calls) {
				if err := emit(sse(chatChunk(cid, created, model,
					pyval.Obj{{"tool_calls", piece}}, nil))); err != nil {
					return err
				}
			}
			if err := emit(sse(chatChunk(cid, created, model, pyval.Obj{}, "tool_calls"))); err != nil {
				return err
			}
		} else {
			if buf != "" {
				if err := paceEmit(buf, 2, 12*time.Millisecond, pieceFn(c, cid, created, model, emit)); err != nil {
					return err
				}
			}
			if err := emit(sse(chatChunk(cid, created, model, pyval.Obj{}, "stop"))); err != nil {
				return err
			}
		}
	}
	if wantUsage(req.StreamOptions) {
		if err := emit(sse(pyval.Obj{
			{"id", cid}, {"object", "chat.completion.chunk"}, {"created", created},
			{"model", model}, {"choices", []any{}},
			{"usage", pyval.Obj{{"prompt_tokens", 0}, {"completion_tokens", 0},
				{"total_tokens", 0}}},
		})); err != nil {
			return err
		}
	}
	if err := emit("data: [DONE]\n\n"); err != nil {
		return err
	}
	a.Store.Mark(accID, true, "")
	return nil
}

func sseErrChunk(err error, accID string, a *App) pyval.Obj {
	var ae *engine.ErrAuth
	var ge *engine.ErrGeneration
	switch {
	case errors.As(err, &ae):
		a.Store.Mark(accID, false, ae.Error())
		return pyval.Obj{{"error", pyval.Obj{
			{"message", ae.Error()}, {"type", "auth_error"}, {"code", 401}}}}
	case errors.As(err, &ge):
		a.Store.Mark(accID, true, "助手超时: "+truncate(ge.Error(), 60))
		// Python 这里跑在流式生成器所在线程，该线程的 thread-local page 与本任务无关，
		// 相当于空上下文上的 no-op。Go 用空 PageCtx 复刻，绝不能去动别的任务的页面。
		a.Engine.ResetThread(a.Engine.NewPageCtx(), false)
		return pyval.Obj{{"error", pyval.Obj{
			{"message", ge.Error()}, {"type", "server_error"}, {"code", 502}}}}
	}
	return pyval.Obj{{"error", pyval.Obj{
		{"message", "内部错误: " + err.Error()}, {"type", "server_error"}, {"code", 500}}}}
}

func sseHeaderMap() map[string]string {
	out := map[string]string{"content-type": "text/event-stream"}
	for _, kv := range sseHeaders {
		out[kv[0]] = kv[1]
	}
	return out
}

func toolCallDeltas(calls []pyval.Obj) []any {
	head := make([]any, 0, len(calls))
	body := make([]any, 0, len(calls))
	for i, call := range calls {
		fn := funcVal(call, "function")
		head = append(head, pyval.Obj{
			{"index", i}, {"id", funcVal(call, "id")}, {"type", "function"},
			{"function", pyval.Obj{{"name", funcVal(fn, "name")},
				{"arguments", ""}}},
		})
		body = append(body, pyval.Obj{
			{"index", i},
			{"function", pyval.Obj{{"arguments", funcVal(fn, "arguments")}}},
		})
	}
	return []any{head, body}
}

// funcVal 从「有序对象」或普通 map 里按键取值。
func funcVal(v any, key string) any {
	switch t := v.(type) {
	case pyval.Obj:
		for _, p := range t {
			if p.Key == key {
				return p.Val
			}
		}
	case map[string]any:
		return t[key]
	}
	return nil
}

// --- Responses API（新版 Codex 默认走这个端点） ---

func (a *App) responsesAPI(c *reqCtx) (any, error) {
	body, err := decodeBody(c.R)
	if err != nil {
		return nil, err
	}
	req, errs := parseResponsesRequest(body)
	if len(errs) > 0 {
		return nil, a.validationFailed(c.R, errs)
	}
	prompt := buildChatPrompt(responsesMessages(req))
	if prompt == "" {
		return nil, httpError(400, "input 为空")
	}
	model := resolveModel(optStr(req.Model), "muse-spark")
	timeout := int(pyNumOr(req.Timeout, int64(a.CFG.ChatTimeout)))
	acc := a.Store.PickAccount(a.Engine.CurrentAccountID(), true, false, "", nil, true)
	if acc == nil {
		return nil, httpError(400, "没有可用账号，请先在管理页面导入 cookie")
	}
	rid := "resp_" + uuidHex(24)
	mid := "msg_" + uuidHex(24)
	created := a.now().Unix()
	accID := strOf(acc["id"])
	cookies := toStringMap(acc["cookies"])
	expires := cookieIntMap(acc["cookies_exp"])

	envelope := func(status, text string) pyval.Obj {
		done := status == "completed"
		msgStatus := "in_progress"
		if done {
			msgStatus = "completed"
		}
		var content []any
		if text != "" {
			content = []any{pyval.Obj{{"type", "output_text"}, {"text", text},
				{"annotations", []any{}}}}
		}
		return pyval.Obj{
			{"id", rid}, {"object", "response"}, {"created_at", created},
			{"status", status}, {"model", model},
			{"output", []any{pyval.Obj{{"type", "message"}, {"id", mid},
				{"role", "assistant"}, {"status", msgStatus}, {"content", content}}}},
			{"usage", pyval.Obj{{"input_tokens", 0}, {"output_tokens", 0},
				{"total_tokens", 0}}},
		}
	}

	if req.Stream {
		stop := make(chan struct{})
		go func() {
			<-c.R.Context().Done()
			close(stop)
		}()
		return &rawResponse{Status: http.StatusOK, Headers: sseHeaderMap(),
			Stream: func(cc *reqCtx) error {
				flusher, _ := cc.W.(http.Flusher)
				emit := func(ev string, data pyval.Obj) error {
					if _, err := cc.W.Write([]byte(sseEvent(ev, data))); err != nil {
						return err
					}
					if flusher != nil {
						flusher.Flush()
					}
					return nil
				}
				if err := emit("response.created", pyval.Obj{
					{"type", "response.created"}, {"response", envelope("in_progress", "")}}); err != nil {
					return err
				}
				if err := emit("response.output_item.added", pyval.Obj{
					{"type", "response.output_item.added"}, {"output_index", 0},
					{"item", pyval.Obj{{"id", mid}, {"type", "message"}, {"role", "assistant"},
						{"status", "in_progress"}, {"content", []any{}}}}}); err != nil {
					return err
				}
				if err := emit("response.content_part.added", pyval.Obj{
					{"type", "response.content_part.added"}, {"item_id", mid},
					{"output_index", 0}, {"content_index", 0},
					{"part", pyval.Obj{{"type", "output_text"}, {"text", ""},
						{"annotations", []any{}}}}}); err != nil {
					return err
				}
				full := ""
				for it := range a.safeChatStream(accID, cookies, expires, prompt, timeout, stop) {
					if it.Err != nil {
						return emit("response.failed", pyval.Obj{
							{"type", "response.failed"},
							{"response", envelope("failed", "")}})
					}
					full += it.Text
					if err := emit("response.output_text.delta", pyval.Obj{
						{"type", "response.output_text.delta"}, {"item_id", mid},
						{"output_index", 0}, {"content_index", 0},
						{"delta", it.Text}}); err != nil {
						return err
					}
				}
				if err := emit("response.output_text.done", pyval.Obj{
					{"type", "response.output_text.done"}, {"item_id", mid},
					{"output_index", 0}, {"content_index", 0}, {"text", full}}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", pyval.Obj{
					{"type", "response.output_item.done"}, {"output_index", 0},
					{"item", pyval.Obj{{"id", mid}, {"type", "message"}, {"role", "assistant"},
						{"status", "completed"}, {"content", []any{pyval.Obj{
							{"type", "output_text"}, {"text", full},
							{"annotations", []any{}}}}}}}}); err != nil {
					return err
				}
				return emit("response.completed", pyval.Obj{
					{"type", "response.completed"},
					{"response", envelope("completed", full)}})
			}}, nil
	}

	text, err := collectChat(a.safeChatStream(accID, cookies, expires, prompt, timeout, nil))
	if err != nil {
		return nil, mapGenErr(err)
	}
	return envelope("completed", text), nil
}

// sseEvent 对应 _sse_event()。
func sseEvent(event string, data pyval.Obj) string {
	return "event: " + event + "\ndata: " + pyval.Dumps(data, false, false) + "\n\n"
}

// pieceFn 是 _pace_text 每片要落的那件事：包一个 chat.completion.chunk 的 SSE 帧。
func pieceFn(c *reqCtx, cid string, created int64, model string,
	emit func(string) error) func(string) error {
	return func(piece string) error {
		return emit(sse(chatChunk(cid, created, model,
			pyval.Obj{{"content", piece}}, nil)))
	}
}
