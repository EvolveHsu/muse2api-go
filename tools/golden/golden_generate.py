"""录 engine.py 生成/对话流程的 CDP 调用序列，作为 Go 侧回放比对的金样本。

假 page 按"表达式特征"分派返回值（避免被 _scroll_bottom 这类隐藏 JS 调用打乱队列），
同时按真实顺序记录每次 send/js 与 js 的返回值。Go 测试用同一份记录回放：
输入完全相同，逐条比对表达式序列与最终结果 —— 顺序或参数错一位就会被抓到。
"""
import base64
import json
import os
import sys

sys.path.insert(0, "/app")
import engine as E  # noqa: E402

for d in ("/tmp/genc/data/downloads", "/tmp/genc/data/media", "/tmp/genc/profile"):
    os.makedirs(d, exist_ok=True)

FIXED = [1700000000.0]


class FakeTime:
    @staticmethod
    def time():
        return FIXED[0]

    @staticmethod
    def sleep(s=0):
        FIXED[0] += float(s or 0)

    @staticmethod
    def monotonic():
        return FIXED[0]


E.time = FakeTime

EXPRS = []
EXPR_IDX = {}


def intern(expr):
    if expr not in EXPR_IDX:
        EXPR_IDX[expr] = len(EXPRS)
        EXPRS.append(expr)
    return EXPR_IDX[expr]


# 特征顺序敏感：先匹配的优先。标记必须取自真实 JS 片段，否则会静默落到默认值，
# 于是"分支没被驱动"却看不出问题（上一版就是这么把 state 误判成 agent_cnt 的）。
RULES = [
    ("data-hatch-shell-hydration-state", "ws_ready"),
    ("slice(-700)", "state"),                        # _wait_attachment 的状态探测
    ("cnt:nonEmpty.length,total:bs.length", "state"),  # _POLL_CHAT_JS
    ("bubbleCount >= 24", "reset_nav"),
    ("HTMLTextAreaElement.prototype", "setter"),
    ("r.left+r.width/2", "rect"),
    ("var sel=", "click_pt"),                        # _CLICK_JS（quota 点 Settings 用 r.x/r.y）
    ("role=menuitem", "menu_item"),                  # quota 第二步：找文本为 Settings 的菜单项
    ("/发送|send/i.test", "send_btn"),
    ("btn:b?(b.disabled?2:1):0", "send_state"),
    ("new DataTransfer", "inject"),
    ("var hasBtn", "attached"),
    ("remove attachment", "clear"),
    ("seen.add", "atts"),
    ("closest('[data-testid^=\"hatch-chat-attachment", "dl_clicked"),
    ("b.scrollHeight-a.scrollHeight", "scroll"),
    ("aria-modal=true", "dialog_txt"),
    ("JSON.stringify({atts:", "dump"),
    ("document.body.innerText.slice(-500)", "tail"),
    ("document.body.innerText.slice(0,1200)", "body"),
    ("!!document.querySelector('textarea')", "has_ta"),
    ("chat-user-bubble", "user_cnt"),
    ("return bs[i].innerText||''", "agent_text"),
    ("n++;}", "agent_cnt"),
]

DEFAULTS = {"scroll": 1, "has_ta": True, "user_cnt": 0, "agent_cnt": 0, "ws_ready": "ready",
            "body": "", "tail": "", "dump": "{}", "agent_text": ""}


def rule_of(expr):
    for mark, name in RULES:
        if mark in expr:
            return name
    # _poll_chat 与 _wait_attachment 的状态 JS 都以 agent bubble 为特征，用额外标记区分
    if "stop" in expr and "cnt" in expr:
        return "state"
    return "unknown:" + expr[:40]


ALL_PAGES = []


class Script:
    def __init__(self, queues=None):
        self.queues = {k: list(v) for k, v in (queues or {}).items()}
        self.last = {}
        self.log = []
        self.fallbacks = []
        ALL_PAGES.append(self)

    def value_for(self, r):
        q = self.queues.get(r)
        if q:
            v = q.pop(0)
            self.last[r] = v
            return v
        if r in self.last:      # 轮询规则：耗尽后粘住最后一个值（"它一直在问同一件事"）
            return self.last[r]
        if r in DEFAULTS:
            self.fallbacks.append(r)   # 静默走默认值 = 该分支其实没被驱动，最后统一检查
            return DEFAULTS[r]
        raise AssertionError("没有为规则 %s 配值" % r)

    def js(self, expr, await_promise=False, timeout=None):
        r = rule_of(expr)
        v = self.value_for(r)
        i = intern(expr)
        if isinstance(v, dict) and v.get("__raise__"):
            self.log.append(["js", i, await_promise, None, True])
            raise RuntimeError("boom")
        self.log.append(["js", i, await_promise, v, False])
        return v

    def send(self, method, params=None, timeout=None):
        self.log.append(["send", method, params])
        return {"result": {}}

    def close(self):
        self.log.append(["close", None, None])


class Cfg:
    chromium = "/usr/bin/chromium"
    cdp_port = 19210
    home_dir = "/root"
    extra_path = "/snap/bin"
    profile_dir = "/tmp/genc/profile"
    data_dir = "/tmp/genc/data"
    download_dir = "/tmp/genc/data/downloads"
    media_dir = "/tmp/genc/data/media"
    site_url = "https://muse.ai/"
    login_wait = 60
    image_timeout = 240
    video_timeout = 600
    chat_timeout = 300


def fresh(queues=None):
    eng = E.MuseEngine(Cfg())
    page = Script(queues)
    eng.page = page
    return eng, page


out = {}
ATT = "hatch-chat-attachment-presentation-"
att_img = json.dumps([{"tid": ATT + "image", "hasVideo": False, "src": "https://x/a.png",
                       "vSrc": "", "iSrc": "https://x/a.png", "w": 512, "h": 512}])
att_vid = json.dumps([{"tid": ATT + "video", "hasVideo": True, "src": "https://x/a.mp4",
                       "vSrc": "https://x/a.mp4", "iSrc": "", "w": 0, "h": 0}])
st = lambda txt, stop, cnt=1: json.dumps({"cnt": cnt, "txt": txt, "stop": stop, "tail": ""})  # noqa: E731

# ---------------- attachments ----------------
e, p = fresh({"atts": [json.dumps([{"tid": ATT + "image", "src": "https://x/a.png", "w": 10, "h": 20}])]})
out["attachments_valid"] = {"ret": e.attachments(), "log": p.log}
e, p = fresh({"atts": ["{坏 json"]})
out["attachments_badjson"] = {"ret": e.attachments(), "log": p.log}
e, p = fresh({"atts": [{"__raise__": True}]})
out["attachments_raise"] = {"ret": e.attachments(), "log": p.log}

# ---------------- _send ----------------
e, p = fresh({"rect": ['{"x":100,"y":200}'], "setter": [7], "send_btn": ["clicked"]})
out["send_clicked"] = {"ret": e._send("你好"), "log": p.log}

e, p = fresh({"rect": ['{"x":1,"y":2}'], "setter": [5],
              "send_btn": ["no-button", "disabled", "no-button", "no-button", "no-button"],
              "has_ta": ["", ""]})
out["send_enter_fallback"] = {"ret": e._send("你好"), "log": p.log}

e, p = fresh({"rect": [None], "setter": [0], "send_btn": ["no-button"] * 8, "has_ta": ["x"]})
try:
    r = e._send("你好")
    out["send_no_rect"] = {"ret": r, "log": p.log}
except Exception as exc:  # noqa: BLE001
    out["send_no_rect"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

# ---------------- _poll_chat / counts ----------------
e, p = fresh({"state": [json.dumps({"cnt": 3, "txt": "你好", "stop": True})]})
out["poll_chat"] = {"ret": list(e._poll_chat()), "log": p.log}
e, p = fresh({"agent_cnt": ["7"]})
out["agent_count"] = {"ret": e._agent_count(), "log": p.log}
e, p = fresh({"user_cnt": [None]})
out["user_count_none"] = {"ret": e._user_count(), "log": p.log}

# ---------------- _wait_attachment ----------------
e, p = fresh({"atts": [att_img], "state": [st("生成中", True)]})
out["wait_att_immediate"] = {"ret": e._wait_attachment("", 30, "image", base_att_cnt=0),
                             "log": p.log}

e, p = fresh({"atts": [att_vid, att_vid, att_vid], "state": [st("x", True)] * 3})
out["wait_att_video_stable"] = {"ret": e._wait_attachment("", 30, "video", base_att_cnt=0),
                                "log": p.log}

e, p = fresh({"atts": ["[]"], "state": [json.dumps(
    {"cnt": 1, "txt": "好", "stop": False, "tail": "out of credits"})]})
try:
    e._wait_attachment("", 30, "image", base_att_cnt=0)
    out["wait_att_credits"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["wait_att_credits"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

only = st("我不能生成", False, cnt=2)
e, p = fresh({"atts": ["[]"] * 25, "state": [only] * 25})
try:
    e._wait_attachment("", 60, "image", base_att_cnt=0)
    out["wait_att_textonly"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["wait_att_textonly"] = {"err": type(exc).__name__ + ":" + str(exc)}

report = st("verified generated artifact webp", True, cnt=2)
e, p = fresh({"atts": ["[]"] * 3 + [att_img], "state": [report] * 5})
out["wait_att_media_report"] = {"ret": e._wait_attachment("", 30, "image", base_att_cnt=0),
                                "log": p.log}

# 基线 src 相同的附件不能被当新结果
same = json.dumps([{"tid": ATT + "image", "hasVideo": False, "src": "https://x/same",
                    "vSrc": "", "iSrc": "", "w": 9, "h": 9}])
e, p = fresh({"atts": [same, same, same], "state": [st("a", True)] * 4})
out["wait_att_baseline_excluded"] = {"ret": e._wait_attachment("", 3, "image", base_att_cnt=0,
                                                              baseline_sources=["https://x/same"]),
                                     "log": p.log}

# ---------------- extract_bytes ----------------
b64 = base64.b64encode(b"\x00\x01binary").decode()
payload = json.dumps({"ok": True, "mime": "image/webp", "size": 8,
                      "url": "https://x/a", "b64": b64})
e, p = fresh({})
p.js = lambda expr, await_promise=False, timeout=None: (
    p.log.append(["js", intern(expr), await_promise, payload, False]) or payload)
data, mime, url = e.extract_bytes("https://x/a")
out["extract_ok"] = {"bytes_b64": base64.b64encode(data).decode(), "mime": mime, "url": url,
                     "log": p.log}

e, p = fresh({})
n = {"i": 0}


def fail_js(expr, await_promise=False, timeout=None):
    n["i"] += 1
    p.log.append(["js", intern(expr), await_promise, json.dumps({"ok": False, "err": "media-http-404"}), False])
    return json.dumps({"ok": False, "err": "media-http-404"})


p.js = fail_js
try:
    e.extract_bytes("https://x/a")
    out["extract_fail"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["extract_fail"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log,
                           "js_calls": n["i"]}

# ---------------- chat_stream ----------------
# 注意消费顺序：先取基准 (_poll_chat)，再等新气泡出现，再进入流式增量循环。
e, p = fresh({"state": [st("", False, cnt=0), st("你", True, cnt=1), st("你好", True, cnt=1),
                        st("你好", False, cnt=1), st("你好", False, cnt=1),
                        st("你好", False, cnt=1), st("你好", False, cnt=1)]})
e.ensure_page = lambda *a, **k: p
e.reset_thread = lambda *a, **k: None
e._send = lambda prompt: "clicked"
out["chat_stream"] = {"deltas": list(e.chat_stream({"a": "1"}, "hi", timeout=30)),
                      "log": p.log}

e, p = fresh({"state": [st("", False, cnt=0)] * 300,
              "tail": ["Connecting..."] * 300})
e.ensure_page = lambda *a, **k: p
e.reset_thread = lambda *a, **k: None
e._send = lambda prompt: "clicked"
try:
    list(e.chat_stream({"a": "1"}, "hi", timeout=30))
    out["chat_stream_connecting"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["chat_stream_connecting"] = {"err": type(exc).__name__ + ":" + str(exc),
                                     "tail_calls": len(p.last.get("tail") or []) if isinstance(
                                         p.last.get("tail"), list) else None}

# ---------------- generate（编排层，内部打桩） ----------------
e, p = fresh({})
calls = []
e.ensure_page = lambda *a, **k: p
e.reset_thread = lambda *a, **k: None
e._scroll_bottom = lambda: None
e._clear_attachments = lambda: None
e._agent_count = lambda: 0
e._send = lambda prompt: calls.append(("send", prompt)) or "clicked"
e._wait_attachment = lambda *a, **k: calls.append(("wait_timeout", a[1])) or {
    "tid": ATT + "image", "src": "https://x/a.png", "vSrc": "", "w": 8, "h": 8}
e.extract_bytes = lambda src, expect="image": (calls.append(("extract", src)) or
                                               (b"\x00bin", "image/png", "https://x/a.png"))
e.attachments = lambda: calls.append(["atts"]) or [{"src": "old", "vSrc": "old", "iSrc": "old"}]
res = e.generate({"a": "1"}, "画猫", expect="image", timeout=60)
out["generate_blob"] = {"calls": calls, "kind": res["kind"], "via": res["via"], "ext": res["ext"],
                        "size": res["size"], "w": res["w"], "h": res["h"],
                        "ret_keys": sorted(res.keys()),
                        "name_shape": ["<hex32>", res["name"][-4:]] if len(res["filename"]) == 37
                        else ["BAD", res["filename"]]}

e, p = fresh({})
e.ensure_page = lambda *a, **k: p
e.reset_thread = lambda *a, **k: None
e._scroll_bottom = lambda: None
e._clear_attachments = lambda: None
e._agent_count = lambda: 0
e._send = lambda prompt: "clicked"
e._wait_attachment = lambda *a, **k: {"tid": ATT + "video", "src": "https://x/v.mp4",
                                      "vSrc": "https://x/v.mp4", "w": 0, "h": 0}
e.attachments = lambda: []
e.extract_bytes = lambda src, expect="image": (_ for _ in ()).throw(
    E.MuseGenerationError("取不回"))
mp = "/tmp/genc/data/downloads/real.mp4"
open(mp, "wb").write(b"vidata")
e._download_fallback = lambda src, timeout=180: mp
res2 = e.generate({"a": "1"}, "视频", expect="video", timeout=60)
out["generate_download_fallback"] = {"via": res2["via"], "ext": res2["ext"],
                                     "kind": res2["kind"], "size": res2["size"],
                                     "ret_keys": sorted(res2.keys())}

e, p = fresh({})
e.ensure_page = lambda *a, **k: p
e.reset_thread = lambda *a, **k: None
e._scroll_bottom = lambda: None
e._clear_attachments = lambda: None
e._agent_count = lambda: 0
e._send = lambda prompt: "clicked"
e.attachments = lambda: []
e._wait_attachment = lambda *a, **k: None
try:
    e.generate({"a": "1"}, "画猫", timeout=60)
    out["generate_no_attachment"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_no_attachment"] = {"err": type(exc).__name__ + ":" + str(exc)}

e, p = fresh({})
e.ensure_page = lambda *a, **k: p
e.reset_thread = lambda *a, **k: None
e._scroll_bottom = lambda: None
e._clear_attachments = lambda: None
e._agent_count = lambda: 0
e._send = lambda prompt: "clicked"
e.attachments = lambda: []
e._wait_attachment = lambda *a, **k: {"tid": ATT + "image", "src": "https://x/a",
                                      "vSrc": "", "w": 4, "h": 4}
e.extract_bytes = lambda src, expect="image": (b"x", "", "")
e._download_fallback = lambda src, timeout=180: None
e._attach_image = lambda img: None
try:
    e.generate({"a": "1"}, "画猫", timeout=60, reference_image="https://x/ref.png")
    out["generate_lost_file"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_lost_file"] = {"err": type(exc).__name__ + ":" + str(exc)}

# ---------------- quota ----------------
e, p = fresh({"click_pt": ['{"x":10,"y":20}'], "menu_item": ['{"x":30,"y":40}'], "dialog_txt": [
    "Free plan\nWeekly limit resets on Sep 30\n1% used\nAdditional tokens\nNever expires\n0% used (2B tokens left)"]})
e.ensure_page = lambda *a, **k: p
q = e.quota({"a": "1"})
out["quota"] = {"ret": q, "sends": [x for x in p.log if x[0] == "send"]}

fb = {}
for pg in ALL_PAGES:
    for r in pg.fallbacks:
        fb[r] = fb.get(r, 0) + 1
out["__fallbacks__"] = fb

print("@@JSON@@")
print(base64.b64encode(json.dumps({"sections": out, "exprs": EXPRS}, ensure_ascii=False)
                       .encode("utf-8")).decode())
