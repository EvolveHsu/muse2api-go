"""录 muse2api（Python 版）对外 HTTP 契约，供 Go 版逐状态码/逐键回放。

安全边界（这条脚本会打到**现役实例**，所以只发这两类请求）：
  1. 纯读 GET；
  2. 一定在进 handler 之前就被拒的请求 —— 错 token（Depends(auth) 先拦）
     或缺必填字段（pydantic 422）或空 prompt 的 400。handler 一次都不会执行，
     因此不会起浏览器、不会花额度、不会改账号库。
凡是 body 合法、会真的走到生成/写库的路径，一律不发。

输出里所有密钥类字段（cookies / api_key / cookie_header ...）先 REDACT 再离开容器；
list 型接口只记「类型 + 键集合」不记真实内容，免得把线上状态写进金样本。
"""
import base64
import hashlib
import json
import sys

sys.path.insert(0, "/app")
import requests  # noqa: E402

import config as C  # noqa: E402

CFG = C.CFG
BASE = "http://127.0.0.1:%d" % CFG.port
GOOD = {"Authorization": "Bearer " + (CFG.api_key or "")}
BAD = {"Authorization": "Bearer definitely-not-the-key"}

SECRET_KEYS = {"cookies", "cookies_exp", "api_key", "cookie_header",
               "authorization", "token", "key", "batch"}
# date 每次都不同；content-length 由 body 决定。两边都不进比对。
DROP_HEADERS = {"date", "content-length", "connection", "server"}


def redact(v):
    if isinstance(v, dict):
        return {k: ("<REDACTED>" if k.lower() in SECRET_KEYS else redact(x))
                for k, x in v.items()}
    if isinstance(v, list):
        return [redact(x) for x in v]
    return v


def shape(v):
    """只留结构：类型、键集合、列表元素个数与第一个元素的键集合。"""
    if isinstance(v, dict):
        return {"__type__": "object", "__keys__": list(v.keys()),
                "__items__": {k: shape(x) for k, x in v.items()}}
    if isinstance(v, list):
        return {"__type__": "array", "__n__": len(v),
                "__first__": shape(v[0]) if v else None}
    return {"__type__": type(v).__name__, "__value__": v if not isinstance(v, str)
            else v[:60]}


CASES = []


def case(name, method, path, mode="good", body=None, json_body=True, record="full",
         headers=None, raw=None):
    CASES.append({"name": name, "method": method, "path": path, "auth": mode,
                  "body": body, "json_body": json_body, "record": record,
                  "headers": headers or {}, "raw": raw})


# ---------------- 读接口 ----------------
case("healthz", "GET", "/healthz", "none")
case("readyz", "GET", "/readyz", "none")
case("models_anon", "GET", "/v1/models", "none")
case("models_bad", "GET", "/v1/models", "bad")
case("models_ok", "GET", "/v1/models", "good")
case("media_missing", "GET", "/v1/media/muse2api-does-not-exist.png", "none")
case("media_traversal", "GET", "/v1/media/..%2Faccounts.json", "none", record="status")
case("media_dotdot_plain", "GET", "/v1/media/..%2f..%2fetc%2fpasswd", "none", record="status")
case("media_utf8_name", "GET", "/v1/media/%E7%8C%AB.png", "none", record="status")
# 反斜杠与「..」子串走的是 handler 里的 400 非法文件名；编码斜杠在路由层就 404。
# Go 的 http.ServeMux 会把这类路径 301 到清理后的地址 —— 那是不一致，必须钉住。
case("media_backslash", "GET", "/v1/media/a%5Cb.png", "none", record="status")
case("media_backslash_traversal", "GET", "/v1/media/..%5C..%5Capp.py", "none", record="status")
case("media_dots_only", "GET", "/v1/media/%2E%2E.png", "none", record="status")
case("media_enc_slash_only", "GET", "/v1/media/%2F.png", "none", record="status")
case("media_space_name", "GET", "/v1/media/a%20b.png", "none", record="status")
case("task_missing", "GET", "/v1/images/tasks/nope", "good")
case("video_missing", "GET", "/v1/videos/nope", "good")
case("video_gen_missing", "GET", "/v1/videos/generations/nope", "good")
case("status_ok", "GET", "/admin/status", "good", record="shape")
case("accounts_ok", "GET", "/admin/accounts", "good", record="shape")
case("tasks_limit", "GET", "/admin/tasks?limit=5", "good", record="shape")
case("tasks_bad_limit", "GET", "/admin/tasks?limit=abc", "good")
case("media_list", "GET", "/admin/media", "good", record="shape")
case("apikey_ok", "GET", "/admin/apikey", "good", record="shape")
case("cookie_helper", "GET", "/admin/cookie-helper", "none", record="blob")
case("cookie_helper_dl", "GET", "/admin/cookie-helper?download=1", "none", record="blob")
case("extension", "GET", "/admin/extension", "none", record="blob")
case("extension_files", "GET", "/admin/extension/files", "none", record="shape")
case("keepalive_status", "GET", "/admin/keepalive/status", "good", record="shape")
case("update_check", "GET", "/admin/update/check", "none", record="shape")
case("repo_status", "GET", "/admin/repo/status", "none", record="shape")
case("index", "GET", "/", "none", record="blob")
case("admin_page", "GET", "/admin", "none", record="blob")
case("openapi", "GET", "/openapi.json", "none", record="openapi")
case("docs", "GET", "/docs", "none", record="blob")
case("v1_unmatched", "GET", "/v1/nope", "good")
case("admin_unmatched", "GET", "/admin/nope", "good")
case("anon_admin", "GET", "/admin/status", "none", record="shape")

# ---------------- 路由器自身的边角（2026-10-06 D3 之后新增，等下次复录补上） ----------------
# 这几条都不是业务接口，而是 FastAPI/Starlette 的对外行为，Go 侧一并复刻了：
#   /redoc 与 /docs/oauth2-redirect 是 app 装配时白挂的另两条自动路由；
#   HEAD 由 Starlette 自动加进 GET 路由的 methods；
#   尾斜杠补/削后能匹配就 307（redirect_slashes），Location 是绝对地址、含查询串；
#   路径命中但方法不对 → 405，且带 Allow 头（APIRoute.handle 抛的 HTTPException 带 headers）。
case("redoc", "GET", "/redoc", "none", record="blob")
case("oauth2_redirect", "GET", "/docs/oauth2-redirect", "none", record="blob")
case("head_models", "HEAD", "/v1/models", "good")
case("slash_redirect_models", "GET", "/v1/models/?x=1", "good")
case("slash_redirect_admin", "GET", "/admin/", "none")
case("method_not_allowed_gen", "GET", "/v1/images/generations", "good")

# ---------------- 参数校验与鉴权（都在进 handler 前被拒） ----------------
case("images_gen_noauth", "POST", "/v1/images/generations", "none", {"prompt": "x"})
case("images_gen_bad", "POST", "/v1/images/generations", "bad", {"prompt": "x"})
case("images_gen_empty", "POST", "/v1/images/generations", "good", {})
case("images_gen_prompt_type", "POST", "/v1/images/generations", "good", {"prompt": 123})
case("images_gen_timeout", "POST", "/v1/images/generations", "good",
     {"prompt": "x", "timeout": 999})
case("images_tasks_empty", "POST", "/v1/images/tasks", "good", {})
case("images_tasks_bad", "POST", "/v1/images/tasks", "bad", {})
case("videos_empty", "POST", "/v1/videos", "good", {})
case("videos_gen_empty", "POST", "/v1/videos/generations", "good", {})
case("videos_bad", "POST", "/v1/videos", "bad", {})
case("chat_empty", "POST", "/v1/chat/completions", "good", {})
case("chat_bad_msgs", "POST", "/v1/chat/completions", "good", {"messages": "x"})
case("chat_bad_role", "POST", "/v1/chat/completions", "good",
     {"messages": [{"content": "hi"}]})
case("chat_noauth", "POST", "/v1/chat/completions", "none", {"messages": []})
case("responses_empty", "POST", "/v1/responses", "good", {})
case("responses_bad_input", "POST", "/v1/responses", "good", {"input": {}})
case("responses_noauth", "POST", "/v1/responses", "none", {})
# edits 手工解析 body：空 body 会被补默认提示词然后真的去生成，所以只发坏 JSON。
case("edits_badjson", "POST", "/v1/images/edits", "good", raw="{坏 json",
     headers={"Content-Type": "application/json"})
case("edits_noauth", "POST", "/v1/images/edits", "bad", raw="{}",
     headers={"Content-Type": "application/json"})

# 写接口一律只用坏 token 探 401（handler 绝不会执行）
for m, p in [("POST", "/admin/accounts"), ("PATCH", "/admin/accounts/capnope"),
             ("DELETE", "/admin/accounts/capnope"), ("POST", "/admin/apikey/rotate"),
             ("POST", "/admin/tasks/clear"), ("DELETE", "/admin/tasks/capnope"),
             ("POST", "/admin/relogin"), ("POST", "/admin/accounts/capnope/test"),
             ("POST", "/admin/accounts/capnope/relogin"),
             ("POST", "/admin/accounts/capnope/cookies"),
             ("POST", "/admin/accounts/capnope/quota"), ("POST", "/admin/quota"),
             ("POST", "/admin/accounts/keepalive"),
             ("POST", "/admin/accounts/capnope/keepalive"),
             ("POST", "/admin/update/upgrade"), ("POST", "/admin/repo/pull"),
             ("POST", "/admin/repo/push")]:
    case("write_%s_%s" % (m.lower(), p.strip("/").replace("/", "_").replace("{", "")),
         m, p, "bad", {})

# ---------------- CORS ----------------
ORIGIN = {"Origin": "https://muse.ai"}
case("cors_get", "GET", "/v1/models", "none", headers=ORIGIN)
case("cors_options", "OPTIONS", "/v1/chat/completions", "none", headers={
    "Origin": "https://muse.ai", "Access-Control-Request-Method": "POST",
    "Access-Control-Request-Headers": "authorization,content-type"})

# FileResponse 的 content-type / content-disposition 是这份契约里唯一「猜不出来」的部分
# （mimetypes 表两边不一样），所以拿线上真实媒体文件各探一次，只记头与摘要。
try:
    _ml = requests.get(BASE + "/admin/media", headers=GOOD, timeout=25).json().get("media") or []
    for _i, _m in enumerate(_ml[:4]):
        case("media_real_%d" % _i, "GET", _m["url"], "none", record="blob")
except Exception as exc:  # noqa: BLE001
    fail.append("media_real probe: %s" % exc)

out = {}
FILES = {}
fail = []
for c in CASES:
    hdr = dict(c["headers"])
    if c["auth"] == "good":
        hdr.update(GOOD)
    elif c["auth"] == "bad":
        hdr.update(BAD)
    kwargs = {}
    if c["raw"] is not None:
        kwargs["data"] = c["raw"].encode("utf8")
    elif c["body"] is not None and c["method"] in ("POST", "PATCH", "PUT"):
        if c["json_body"]:
            kwargs["json"] = c["body"]
        else:
            kwargs["data"] = c["body"]
    try:
        r = requests.request(c["method"], BASE + c["path"], headers=hdr,
                             timeout=25, allow_redirects=False, **kwargs)
    except Exception as exc:  # noqa: BLE001
        fail.append("%s %s -> %s" % (c["method"], c["path"], exc))
        continue
    heads = {k.lower(): v for k, v in r.headers.items() if k.lower() not in DROP_HEADERS}
    row = {"method": c["method"], "path": c["path"], "auth": c["auth"],
           "status": r.status_code, "headers": heads,
           # 回放要能重放同一个请求：把请求体与实际生效的 content-type 也记下来。
           "request_body": c["body"], "request_raw": c["raw"],
           "request_ctype": r.request.headers.get("Content-Type", "")}
    ct = (r.headers.get("content-type") or "").lower()
    if c["record"] == "status":
        # 只留状态码与头：路径穿越这类探针万一真的读到了线上文件，
        # 内容（账号 cookie 等）绝不能进金样本。400+ 的响应体是错误壳，安全且必要。
        row["served_bytes"] = len(r.content)
        if r.status_code >= 400:
            try:
                row["body"] = redact(r.json())
            except Exception:  # noqa: BLE001
                row["body_text"] = r.text[:200]
    elif c["record"] == "blob" or "json" not in ct:
        row["blob_sha256"] = hashlib.sha256(r.content).hexdigest()
        row["blob_len"] = len(r.content)
        row["blob_head"] = r.text[:200] if "text" in ct or "html" in ct else ""
    elif c["record"] == "openapi":
        row["blob_sha256"] = hashlib.sha256(r.content).hexdigest()
        row["blob_len"] = len(r.content)
        doc = r.json()
        row["openapi_keys"] = list(doc.keys())
        row["openapi_paths"] = {p: sorted(doc["paths"][p].keys()) for p in doc["paths"]}
        FILES["internal/server/testdata/openapi.python.json"] = r.content
    elif c["record"] == "shape":
        row["body_shape"] = shape(redact(r.json()))
    else:
        try:
            row["body"] = redact(r.json())
        except Exception:  # noqa: BLE001
            row["body_text"] = r.text[:400]
    out[c["name"]] = row

out["__meta__"] = {"port": CFG.port, "has_api_key": bool(CFG.api_key),
                   "n_cases": len(CASES), "fails": fail}
# run_on_nas.mjs 认这个字段：连同主金样本一起落地的其它文件。
out["__files__"] = {k: base64.b64encode(v).decode() for k, v in FILES.items()}
print("@@JSON@@")
print(base64.b64encode(json.dumps(out, ensure_ascii=False).encode("utf8")).decode())
