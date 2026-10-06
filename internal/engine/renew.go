package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// newUUID 是接缝：connect_attempt_id 要用随机 UUID，测试里换成固定值。
var newUUID = uuidString

func uuidString() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

const sessionURL = "https://muse.ai/api/session"
const vmWakeURL = "https://muse.ai/api/hatch/vm/wake"

// sessionUA 与 Python 里写死的 UA 串一致（muse.ai 会按 UA 判定客户端）。
const sessionUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

// RenewSessionHTTP 直接调用 muse.ai/api/session 续签 hatch_vml(+48h)/hatch_sess(+30d)/hatch_gw(+1y)，
// 并按需调用 /api/hatch/vm/wake 唤醒云端工作区 VM。对应 engine.py 的同名 staticmethod。
//
// DEVIATION: Cookie 头里各对顺序 —— Python 按 dict 插入顺序拼，Go 按名字排序。
// Cookie 头的成员顺序对服务端无语义，金样本比对时只比集合与逐项取值。
func (e *Engine) RenewSessionHTTP(cookies map[string]string, expires map[string]int, wakeVM bool) (*RenewResult, error) {
	curCookies := copyStrMap(cookies)
	curExp := copyIntMap(expires)

	headers := map[string]string{
		"User-Agent":     sessionUA,
		"Origin":         "https://muse.ai",
		"Referer":        "https://muse.ai/thread/new",
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
		"Cookie":         cookieHeader(curCookies),
	}

	resp, err := e.HTTP.Get(sessionURL, headers, 12*time.Second, false)
	if err != nil {
		// 不回显请求内容：异常可能包含带凭据的代理 URL。
		return nil, &ErrGeneration{Msg: fmt.Sprintf(
			"/api/session 网络请求失败 (%s)；请检查服务器网络/代理后重试", kindOf(err))}
	}
	if resp.StatusCode == 401 {
		return nil, &ErrAuth{Msg: "会话认证失败 (/api/session HTTP 401)，请在官网确认登录后重新导入 cookie"}
	}
	if resp.StatusCode != 200 {
		hint := "上游请求未成功，请稍后重试并检查服务器网络"
		if resp.StatusCode == 403 {
			hint = "访问被拒绝，请检查服务器出口/地区/访问限制；不能据此判定 Cookie 失效"
		}
		return nil, &ErrGeneration{Msg: fmt.Sprintf("/api/session HTTP %d：%s", resp.StatusCode, hint)}
	}

	var sj any
	if err := json.Unmarshal(resp.Body, &sj); err != nil {
		return nil, &ErrGeneration{Msg: "/api/session HTTP 200 返回非 JSON；会话状态未确认"}
	}
	obj, isObj := sj.(map[string]any)
	if !isObj || asString(obj["status"]) != "assigned" {
		return nil, &ErrGeneration{Msg: "/api/session HTTP 200 未返回 assigned 会话；请在官网检查账号/工作区状态"}
	}

	for _, c := range resp.Cookies {
		if c.Value != "" {
			curCookies[c.Name] = c.Value
		}
		if c.Expires != 0 {
			curExp[c.Name] = int(float64(c.Expires))
		}
	}

	vmID := asString(obj["vm_id"])
	vmState := asString(obj["vm_state"])
	wakeOK := false
	if wakeVM && vmID != "" && vmState != "DISABLED" {
		headers["Cookie"] = cookieHeader(curCookies)
		body := map[string]any{
			"vm_id":              vmID,
			"retry_count":        0,
			"connect_attempt_id": newUUID(),
		}
		if rw, err := e.HTTP.Post(vmWakeURL, headers, body, 8*time.Second); err == nil {
			wakeOK = rw.StatusCode == 200
		}
	}

	return &RenewResult{
		OK:         asString(obj["status"]) == "assigned",
		Status:     asString(obj["status"]),
		VMID:       vmID,
		VMState:    vmState,
		WakeOK:     wakeOK,
		Cookies:    curCookies,
		CookiesExp: curExp,
	}, nil
}

func copyStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cookieHeader 拼 "k=v; k=v"，跳过空值（对应 Python 的 `if v`）。
func cookieHeader(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if m[k] == "" {
			continue
		}
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}

// kindOf 把错误映射成 Python 的异常类名，用于复刻错误文案。
func kindOf(err error) string {
	var re *ReqError
	if errors.As(err, &re) {
		return re.Kind
	}
	return "RequestException"
}
