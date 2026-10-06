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
	timeout := int(numOrPtr(req.Timeout, int64(a.CFG.ImageTimeout)))
	if req.Async {
		return a.queueImage(c, req, prompt, ref, nil)
	}
	res, _, err := a.runGeneration(prompt, "image", timeout, "", nil, ref, c.R.Context().Done())
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

// imagesEdits 手工解析 body（同时兼容 multipart/form-data 与 application/json），
// 所以它没有 pydantic 的 422 —— 坏 JSON 会一路穿到 500，金样本 edits_badjson 钉着这条。
func (a *App) imagesEdits(c *reqCtx) (any, error) {
	ct := strings.ToLower(c.R.Header.Get("content-type"))
	var (
		asyncMode bool
		prompt    string
		model     = "muse-image"
		size      *string
		ar        *string
		format    = "url"
		timeout   *json.Number
		ref       string
	)
	if strings.Contains(ct, "multipart/form-data") {
		if err := c.R.ParseMultipartForm(32 << 20); err != nil {
			return nil, err
		}
		f := c.R.MultipartForm.Value
		asyncMode = truthy(fv(f, "async"))
		prompt = fv(f, "prompt")
		if v := fv(f, "model"); v != "" {
			model = v
		}
		size, ar = optStr(fv(f, "size")), optStr(fv(f, "aspect_ratio"))
		if v := fv(f, "response_format"); v != "" {
			format = v
		}
		if v := fv(f, "timeout"); v != "" {
			if n, err := parseInt(v); err == nil {
				timeout = &n
			}
		}
		if file, hdr, err := c.R.FormFile("image"); err == nil {
			data, rerr := io.ReadAll(file)
			file.Close()
			if rerr == nil {
				mimeT := hdr.Header.Get("Content-Type")
				if mimeT == "" {
					mimeT = "image/png"
				}
				ref = "data:" + mimeT + ";base64," + encodeB64(data)
			}
		} else if v := fv(f, "image"); v != "" {
			ref = v
		}
	} else {
		// 这个路由的签名是 `request: Request`，FastAPI 不做 body 校验，handler 里自己
		// await request.json() —— 所以坏 JSON 与非对象 body 都是未捕获异常（500），
		// 不走 decodeBody 那套 422。
		body, err := manualJSONBody(c.R)
		if err != nil {
			return nil, err
		}
		if b, ok := body["async"].(bool); ok {
			asyncMode = b
		}
		prompt = strOf(body["prompt"])
		if v := strOf(body["model"]); v != "" {
			model = v
		}
		size, ar = optStr(body["size"]), optStr(body["aspect_ratio"])
		if v := strOf(body["response_format"]); v != "" {
			format = v
		}
		if v, ok := body["timeout"].(json.Number); ok {
			timeout = &v
		}
		ref = refFromImage(body["image"])
		if ref == "" {
			ref = refFromImages(anySliceOf(body["images"]))
		}
		if ref == "" {
			ref = strOf(body["reference_image"])
		}
		if ref == "" {
			ref = strOf(body["image_url"])
		}
	}
	if prompt == "" {
		prompt = "参考此图片并进行生图创作"
	}
	req := &ImageRequest{Prompt: prompt, Model: model, N: json.Number("1"),
		Size: size, AspectRatio: ar, ResponseFormat: format, Timeout: timeout,
		Reference: optStr(ref), Async: asyncMode}
	full := buildImagePrompt(req)
	genTimeout := int(numOrPtr(timeout, int64(a.CFG.ImageTimeout)))
	if asyncMode {
		return a.queueImage(c, req, full, ref, nil)
	}
	res, _, err := a.runGeneration(full, "image", genTimeout, "", nil, ref, c.R.Context().Done())
	if err != nil {
		return nil, mapGenErr(err)
	}
	return a.imageResponse(req, res), nil
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
	timeout := int(numOrPtr(req.Timeout, int64(a.CFG.VideoTimeout)))
	task := a.Store.CreateTask("video", req.Prompt)
	tid := strOf(task["id"])
	a.Store.UpdateTask(tid, map[string]any{"api_prompt": prompt})
	a.Store.UpdateTask(tid, map[string]any{"progress": 10})

	go func() {
		t0 := a.now()
		a.Store.UpdateTask(tid, map[string]any{"status": "processing", "progress": 15})
		res, accID, err := a.runGeneration(prompt, "video", timeout, "",
			func(p int) { a.Store.UpdateTask(tid, map[string]any{"progress": p}) }, ref, nil)
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
	timeout := int(numOrPtr(req.Timeout, int64(a.CFG.ChatTimeout)))
	acc := a.Store.PickAccount(a.Engine.CurrentAccountID(), true, false, "")
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
		a.Engine.ResetThread(false)
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
	timeout := int(numOrPtr(req.Timeout, int64(a.CFG.ChatTimeout)))
	acc := a.Store.PickAccount(a.Engine.CurrentAccountID(), true, false, "")
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
