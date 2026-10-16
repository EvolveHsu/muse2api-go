"""让部署中的真 engine.py 自己吐金样本：argv、cookie 注入参数、开页 HTTP 序列、
续签请求、纯函数结果。

手法：monkeypatch subprocess.Popen / E.CDP / sys.modules["requests"]，把"它打算发出去的
东西"录下来。engine.py 里 renew_session_http 与 _open_page 都是**函数体内 import requests**，
所以必须替换 sys.modules 才能拦住，patch 模块属性没用。全程不启动浏览器、不出网。
"""
import base64
import json
import os
import sys
import types

sys.path.insert(0, "/app")
import engine as E  # noqa: E402

FIXED = 1700000000.5
REAL_REQUESTS = sys.modules["requests"]
out = {}
errors = {}


def section(name, fn):
    try:
        out[name] = fn()
    except Exception as exc:  # noqa: BLE001
        errors[name] = type(exc).__name__ + ": " + str(exc)[:200]


class FakeCfg:
    chromium = "/usr/bin/chromium"
    cdp_port = 19210
    home_dir = "/root"
    extra_path = "/snap/bin"
    profile_dir = "/tmp/gengold/profile"
    data_dir = "/tmp/gengold/data"
    download_dir = "/tmp/gengold/data/downloads"
    site_url = "https://muse.ai/"
    login_wait = 60
    image_timeout = 240
    video_timeout = 600
    chat_timeout = 300


os.makedirs(FakeCfg.profile_dir, exist_ok=True)
os.makedirs(FakeCfg.data_dir, exist_ok=True)

E.time.time = lambda: FIXED
E.time.sleep = lambda *a: None


class Boom(Exception):
    pass


# ---------- 1) chromium argv ----------
def do_argv():
    cap = {}

    def fake_popen(args, **kw):
        cap["argv"] = args
        cap["env_PATH_head"] = (kw.get("env") or {}).get("PATH", "")[:60]
        cap["cwd"] = kw.get("cwd")
        cap["stdout_is_logfile"] = str(getattr(kw.get("stdout"), "name", "")).startswith(
            "/tmp/gengold/data")
        raise Boom("stop")

    real_popen, real_http = E.subprocess.Popen, E.http_json
    E.subprocess.Popen = fake_popen
    E.http_json = lambda url, timeout=5.0: (_ for _ in ()).throw(Boom("no reuse"))
    try:
        E.MuseEngine(FakeCfg()).start()
    except Boom:
        pass
    E.subprocess.Popen = real_popen
    E.http_json = real_http
    return cap


section("argv", do_argv)


# ---------- 2) cookie 注入参数 ----------
class FakeCDP:
    def __init__(self, *a, **k):
        self.args = a
        self.calls = []

    def send(self, method, params=None, timeout=None):
        self.calls.append((method, params))
        return {"result": {}}

    def js(self, expr, await_promise=False, timeout=None):
        self.calls.append(("JS", expr))
        return None

    def close(self):
        self.calls.append(("CLOSE", None))


def do_cookies():
    p = FakeCDP()
    eng = E.MuseEngine(FakeCfg())
    eng._apply_cookies(p, {
        "hatch_sess": "s", "hatch_gw": "", "hatch_vml": "v",
        "hatch_native_auth_device": "d", "extra": "e",
    }, {
        "hatch_sess": 1699000000, "hatch_vml": 1700003600,
        "hatch_gw": 1800000000, "bad": "abc",
    })
    return [c for c in p.calls if c[0] != "JS"]


section("apply_cookies", do_cookies)


# ---------- 3) _open_page 的 HTTP 序列（拦住 sys.modules["requests"]） ----------
class RecResp:
    def __init__(self, payload):
        self._p = payload

    def json(self):
        return self._p


def install_fake_requests(store):
    m = types.ModuleType("requests")

    def rec(kind, url, **kw):
        store.append({"m": kind, "url": url, "kw": {k: v for k, v in kw.items() if k != "timeout"},
                      "timeout": kw.get("timeout")})
        if url.endswith("/json/list"):
            return RecResp([{"id": "T1", "type": "page", "url": "https://muse.ai/thread/new",
                             "webSocketDebuggerUrl": "ws://127.0.0.1:19210/devtools/page/T1"},
                            {"id": "T2", "type": "other", "url": "x"}])
        if "/json/close/" in url:
            return RecResp("Target closed")
        if url.startswith("http://127.0.0.1:19210/json/new"):
            return RecResp({"id": "T3", "webSocketDebuggerUrl": "ws://127.0.0.1:19210/devtools/page/T3"})
        return RecResp({"webSocketDebuggerUrl": "ws://x"})

    m.get = lambda url, timeout=None, **kw: rec("GET", url, timeout=timeout, **kw)
    m.put = lambda url, timeout=None, **kw: rec("PUT", url, timeout=timeout, **kw)
    sys.modules["requests"] = m
    return m


def do_open_page():
    store = []
    real = sys.modules["requests"]
    install_fake_requests(store)
    real_cdp = E.CDP
    E.CDP = FakeCDP
    eng = E.MuseEngine(FakeCfg())
    page = eng._open_page()
    E.CDP = real_cdp
    sys.modules["requests"] = real
    return {"calls": store, "enables": getattr(page, "calls", []),
            "served": {"list": [["T1", "page"], ["T2", "other"]],
                       "new_id": "T3",
                       "ws": "ws://127.0.0.1:19210/devtools/page/T3"}}


section("open_page", do_open_page)


# ---------- 3b) _open_page 的隔离上下文路径（1.6.0 新增） ----------
class FakeBrowser:
    """浏览器级 CDP 会话的最小桩：只实现 _open_page 用到的两条 Target.*。"""

    def __init__(self):
        self.calls = []

    def send(self, method, params=None, timeout=None):
        self.calls.append((method, params))
        if method == "Target.createBrowserContext":
            return {"result": {"browserContextId": "CTX-1"}}
        if method == "Target.createTarget":
            return {"result": {"targetId": "T7"}}
        return {"result": {}}

    def close(self):
        self.calls.append(("CLOSE", None))


def do_open_page_isolated():
    real_cdp = E.CDP
    E.CDP = FakeCDP
    eng = E.MuseEngine(FakeCfg())
    eng.browser = FakeBrowser()
    try:
        page = eng._open_page()
    finally:
        E.CDP = real_cdp
    return {"browser": eng.browser.calls, "enables": getattr(page, "calls", []),
            "ws_url": getattr(page, "args", ("",))[0],
            "context_id": eng.current_context_id,
            "active_contexts": sorted(eng._active_contexts)}


section("open_page_isolated", do_open_page_isolated)


# ---------- 4) renew_session_http ----------
def do_renew(status=200, payload=None, set_cookies=None, wake=True, raise_json=False):
    store = []
    real = sys.modules["requests"]
    m = types.ModuleType("requests")

    class FakeCookie:
        def __init__(self, name, value, expires):
            self.name, self.value, self.expires = name, value, expires

    served = {"status": status, "payload": payload,
              "cookies": [list(c) for c in (set_cookies or [])], "raise_json": raise_json}

    class R:
        def __init__(self, code, p, ck):
            self.status_code, self._p, self.cookies = code, p, ck

        def json(self):
            if raise_json:
                raise ValueError("Expecting value: line 1 column 1 (char 0)")
            return self._p

    def get(url, headers=None, timeout=None, allow_redirects=None):
        store.append({"m": "GET", "url": url, "headers": headers,
                      "timeout": timeout, "allow_redirects": allow_redirects})
        if url == "https://muse.ai/api/session":
            return R(status, payload if payload is not None else {
                "status": "assigned", "vm_id": "vm-1", "vm_state": "SLEEPING"},
                [FakeCookie(*c) for c in (set_cookies or [])])
        return R(200, {}, [])

    def post(url, headers=None, json=None, timeout=None):
        store.append({"m": "POST", "url": url, "headers": headers, "json": json,
                      "timeout": timeout})
        return R(200, {}, [])

    m.get, m.post = get, post
    m.RequestException = REAL_REQUESTS.RequestException
    sys.modules["requests"] = m
    E.requests = m
    try:
        res = E.MuseEngine.renew_session_http({"hatch_sess": "s", "hatch_gw": "g"},
                                              {"hatch_sess": 1800000000}, wake_vm=wake)
        err = None
    except E.MuseAuthError as exc:
        res, err = None, "MuseAuthError:" + str(exc)
    except E.MuseGenerationError as exc:
        res, err = None, "MuseGenerationError:" + str(exc)
    sys.modules["requests"] = real
    E.requests = REAL_REQUESTS
    return {"calls": store, "result": res, "error": err, "served": served}


NEWVML = [["hatch_vml", "NEWVML", 1700086400], ["hatch_empty", "", 0]]
section("renew_ok", lambda: do_renew(set_cookies=NEWVML))
section("renew_401", lambda: do_renew(status=401))
section("renew_403", lambda: do_renew(status=403))
section("renew_200_notjson", lambda: do_renew(raise_json=True))
section("renew_unassigned", lambda: do_renew(payload={"status": "pending"}))
section("renew_no_wake", lambda: do_renew(set_cookies=NEWVML, wake=False))
section("renew_vm_disabled", lambda: do_renew(
    payload={"status": "assigned", "vm_id": "vm-1", "vm_state": "DISABLED"}, set_cookies=NEWVML))
section("renew_500", lambda: do_renew(status=500))

# ---------- 5) 纯函数 ----------
section("pick_ext", lambda: [[m, u, e, E.MuseEngine._pick_ext(m, u, e)] for (m, u, e) in [
    ("image/webp", "", "image"), ("", "https://x/a.mp4", "image"), ("", "", "video"),
    ("video/mp4", "https://x/a.webp", "image"), ("", "https://x/a.png", "image"),
    ("application/octet-stream", "https://x/a", "image"), ("image/jpeg", "https://x/a", "image"),
    ("", "https://x/a.WEBM", "video"), ("Image/PNG", "https://x/a", "image"),
]])
section("parse_quota", lambda: E.MuseEngine._parse_quota(
    "Free plan\nWeekly limit resets on Sep 30\n1% used\nAdditional tokens\nNever expires\n0% used (2B tokens left)"))
section("parse_quota_empty", lambda: E.MuseEngine._parse_quota(""))
section("parse_quota_weird", lambda: E.MuseEngine._parse_quota(
    "Pro plan\n3% used (500M tokens left)\nNever expires\nweekly limit resets on Jan 1"))


def do_normalize():
    res = {"empty": E.MuseEngine._normalize_image("")}
    res["raw_b64"] = E.MuseEngine._normalize_image("iVBORw0KGgo=")
    res["data_plain"] = E.MuseEngine._normalize_image("data:,abc")
    res["data_mime"] = E.MuseEngine._normalize_image("data:image/gif;base64,R0lGOD")
    res["data_mime_nob64"] = E.MuseEngine._normalize_image("data:image/jpeg,XX")
    return res


section("normalize", do_normalize)
out["errors"] = errors
print("@@JSON@@")
print(base64.b64encode(json.dumps(out, ensure_ascii=False).encode("utf-8")).decode())
