package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 这条测试不花额度：账号池是空的，生成必然在 pick_account 那一步就抛
// 「没有可用账号」，正好用来验异步任务面自己的逻辑 —— 202 外壳、幂等键复用、
// 同键不同体 409、队列容量 429、任务状态回写。
func TestImageTaskQueue(t *testing.T) {
	a, _ := testApp(t, "cap-key", time.Unix(1700000000, 0))
	post := func(path, body, idem string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer cap-key")
		if idem != "" {
			r.Header.Set("Idempotency-Key", idem)
		}
		a.ServeHTTP(rec, r)
		return rec
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		// 查询任务同样要带 key：/v1/* 全在 Depends(auth) 后面。
		r.Header.Set("Authorization", "Bearer cap-key")
		a.ServeHTTP(rec, r)
		return rec
	}

	t.Run("202 外壳", func(t *testing.T) {
		created := jsonObj(t, post("/v1/images/tasks", `{"prompt":"猫"}`, "").Body.Bytes())
		if created["status"] != "queued" || created["object"] != "image.task" {
			t.Errorf("外壳不同：%v", created)
		}
		if created["id"] != created["task_id"] {
			t.Errorf("id 与 task_id 应一致：%v / %v", created["id"], created["task_id"])
		}
		if intOf(created["progress"]) != 0 || created["created_at"] == nil {
			t.Errorf("progress/created_at 不同：%v", created)
		}
		tid, _ := created["id"].(string)
		if !strings.HasPrefix(tid, "task_") {
			t.Errorf("任务 id 形状不对：%q", tid)
		}
		// 工人是 goroutine 跑的，失败原因要能落到任务记录里。
		waitFor(t, func() bool {
			body := jsonObj(t, get("/v1/images/tasks/"+tid).Body.Bytes())
			return body["status"] == "failed"
		}, "任务没转成 failed")
		body := jsonObj(t, get("/v1/images/tasks/"+tid).Body.Bytes())
		why := strings.TrimSpace(strOf(body["error"]))
		if why == "" {
			t.Fatalf("失败任务该有 error 字段：%v", body)
		}
		if !strings.Contains(why, "没有可用账号") {
			t.Errorf("错误文案不同：%q", why)
		}
	})

	t.Run("幂等键复用", func(t *testing.T) {
		key := "cap-key-1"
		first := jsonObj(t, post("/v1/images/tasks", `{"prompt":"复用键"}`, key).Body.Bytes())
		second := jsonObj(t, post("/v1/images/tasks", `{"prompt":"复用键"}`, key).Body.Bytes())
		if first["id"] != second["id"] {
			t.Errorf("同一个 Idempotency-Key 该复用任务：%v vs %v", first["id"], second["id"])
		}
		// progress 照抄的是「那一刻」的任务状态：工人已经把它推到 processing 了。
		live := jsonObj(t, get("/v1/images/tasks/"+strOf(first["id"])).Body.Bytes())
		if intOf(second["progress"]) != intOf(live["progress"]) {
			t.Errorf("复用响应的 progress 该等于任务当前进度：响应=%v 任务=%v", second["progress"], live["progress"])
		}
		rec := post("/v1/images/tasks", `{"prompt":"换了内容"}`, key)
		if rec.Code != 409 {
			t.Errorf("同键不同体该 409，得到 %d：%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Idempotency-Key reused with different request") {
			t.Errorf("409 文案不同：%s", rec.Body.String())
		}
		long := make([]rune, 257)
		for i := range long {
			long[i] = 'x'
		}
		rec = post("/v1/images/tasks", `{"prompt":"太长的键"}`, string(long))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Idempotency-Key too long") {
			t.Errorf("超 256 字符的幂等键该 400，得到 %d：%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("队列满 429", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			task := a.Store.CreateTask("image", "占位")
			a.Store.UpdateTask(strOf(task["id"]),
				map[string]any{"status": "processing", "progress": 30})
		}
		rec := post("/v1/images/tasks", `{"prompt":"排队"}`, "")
		if rec.Code != 429 {
			t.Fatalf("队列满该 429，得到 %d：%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Image queue is full; retry later") {
			t.Errorf("429 文案不同：%s", rec.Body.String())
		}
		if !strings.Contains(rec.Header().Get("content-type"), "application/json") {
			t.Errorf("429 该是 JSON 壳：%q", rec.Header().Get("content-type"))
		}
		// 文本任务不占生图额度：kind != image，所以清掉 8 个占位前也不该被算进去。
	})
}

func jsonObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m, ok := decodeJSON(t, b).(map[string]any)
	if !ok {
		t.Fatalf("响应不是 JSON 对象：%s", string(b))
	}
	return m
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("解析失败：%v / %s", err, string(b))
	}
	return v
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
