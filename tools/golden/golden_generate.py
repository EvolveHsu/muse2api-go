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
    ("hatch-dock-more", "quota_settings"),             # 1.6.3：Settings 入口（hatch-dock-more 或中英文按钮）
    ("role=menuitem", "quota_menu"),                   # 1.6.3：设置菜单项（兼容“设定Ctrl+,”等后缀）
    ("block: 'center'", "focus"),                        # _send 第一步：滚进视口并 focus
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
    ("var hasRemove", "attached"),                    # _attach_image 的上传确认轮询
    ("inp.value=''", "clear"),                        # _clear_attachments
    ("seen.add", "atts"),
    ("closest('[data-testid^=\"hatch-chat-attachment", "dl_clicked"),
    ("b.scrollHeight-a.scrollHeight", "scroll"),
    ("aria-modal=true", "dialog_txt"),
    ("JSON.stringify({atts:", "dump"),
    ("document.body.innerText.slice(-500)", "tail"),
    ("document.body.innerText.slice(0,1200)", "body"),
    ("!(document.querySelector('textarea')", "ta_empty"),   # _send 回车兜底后的"输入框已清空"校验
    ("async function(src, expect)", "extract"),         # extract_bytes 的页内 fetch
    ("!!document.querySelector('textarea')", "has_ta"),
    ("chat-user-bubble", "user_cnt"),
    ("return bs[i].innerText||''", "agent_text"),
    ("n++;}", "agent_cnt"),
]

DEFAULTS = {"focus": 1, "scroll": 1, "has_ta": True, "user_cnt": 0, "agent_cnt": 0, "ws_ready": "ready",
            "body": "", "tail": "", "dump": "{}", "agent_text": "",
            "quota_settings": '{"x":10,"y":20}', "quota_menu": '{"x":30,"y":40}'}


def rule_of(expr):
    for mark, name in RULES:
        if mark in expr:
            return name
    # _poll_chat 与 _wait_attachment 的状态 JS 都以 agent bubble 为特征，用额外标记区分
    if "stop" in expr and "cnt" in expr:
        return "state"
    return "unknown:" + expr[:40]


ALL_PAGES = []
# SIDE：某些打桩 JS 调用要顺带在磁盘上造文件（下载兜底就是这样）。Go 侧用同样的
# 「这条 JS 返回之后」时机复现，两边的 listdir 差分才落在同一时刻。
SIDE = {}


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
        i = intern(expr)
        # 先记日志再算值：否则"没有配值"的表达式会被调用方的 except 吞掉，
        # 日志里就永久缺掉这一条（_send 的 focus IIFE 就是这么丢的）。
        self.log.append(["js", i, await_promise, None, False])
        slot = len(self.log) - 1
        try:
            v = self.value_for(r)
        except AssertionError:
            self.fallbacks.append("MISSING:" + r)
            self.log[slot] = ["js", i, await_promise, None, True]
            raise RuntimeError("no value for " + r)
        if isinstance(v, dict) and v.get("__raise__"):
            self.log[slot] = ["js", i, await_promise, None, True]
            raise RuntimeError("boom")
        self.log[slot] = ["js", i, await_promise, v, False]
        cb = SIDE.get(r)
        if cb:
            cb()
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

e, p = fresh({"rect": ['{"x":1,"y":2}'], "setter": [5], "ta_empty": [1],
              "send_btn": ["no-button", "disabled", "no-button", "no-button", "no-button"],
              "has_ta": ["", ""]})
out["send_enter_fallback"] = {"ret": e._send("你好"), "log": p.log}

# 回车兜底后校验 JS 抛错：Python 用 try/except 吞掉，最终仍返回 no-button
e, p = fresh({"rect": ['{"x":1,"y":2}'], "setter": [5],
              "ta_empty": [{"__raise__": True}, {"__raise__": True}],
              "send_btn": ["no-button", "disabled"]})
out["send_enter_raise"] = {"ret": e._send("你好"), "log": p.log}

e, p = fresh({"rect": [None], "setter": [0], "send_btn": ["no-button"] * 8, "has_ta": ["x"]})
try:
    r = e._send("你好")
    out["send_no_rect"] = {"ret": r, "log": p.log}
except Exception as exc:  # noqa: BLE001
    out["send_no_rect"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

# 原型 setter 没写进去时退回 Input.insertText，再补一次 setter。
# Python 的门槛是 len(prompt)（字符数），Go 很容易写成字节数 —— 中文一条就能把它照出来。
e, p = fresh({"rect": ['{"x":9,"y":9}'], "setter": [0, 5], "send_btn": ["clicked"]})
out["send_insert_text"] = {"ret": e._send("hello"), "log": p.log}

e, p = fresh({"rect": ['{"x":9,"y":9}'], "setter": [0, 5], "send_btn": ["clicked"]})
out["send_cjk_200"] = {"ret": e._send("猫" * 200), "log": p.log}

e, p = fresh({"rect": ['{"x":9,"y":9}'], "setter": [0], "send_btn": ["clicked"]})
out["send_cjk_600"] = {"ret": e._send("猫" * 600), "log": p.log}

# ---------------- _poll_chat / counts ----------------
e, p = fresh({"state": [json.dumps({"cnt": 3, "txt": "你好", "stop": True})]})
out["poll_chat"] = {"ret": list(e._poll_chat()), "log": p.log}
e, p = fresh({"agent_cnt": ["7"]})
out["agent_count"] = {"ret": e._agent_count(), "log": p.log}
e, p = fresh({"user_cnt": [None]})
out["user_count_none"] = {"ret": e._user_count(), "log": p.log}

def n_of(p, rule):
    """按规则统计这条假页日志里发生了多少次某类调用 —— 长轮询用例记次数比记整条日志可读。"""
    return sum(1 for x in p.log if x[0] == "js" and rule_of(EXPRS[x[1]]) == rule)


# ---------------- _wait_attachment ----------------
e, p = fresh({"atts": [att_img], "state": [st("生成中", True)]})
out["wait_att_immediate"] = {"ret": e._wait_attachment("", 30, "image", base_att_cnt=0),
                             "log": p.log}

e, p = fresh({"atts": [att_vid, att_vid, att_vid], "state": [st("x", True)] * 3})
out["wait_att_video_stable"] = {"ret": e._wait_attachment("", 30, "video", base_att_cnt=0),
                                "log": p.log}

# 1.6.0 起 expect=video 不再接受封面静帧兜底：页面只有 img 时一直等到超时，返回 None。
# FakeTime 的 sleep 会推进 FIXED，而 Go 的 vclock 从同一个起点开始同样累加 ——
# 起点每例都复位，两边的浮点相位才可比。
FIXED[0] = 1700000000.0
e, p = fresh({"atts": [att_img], "state": [st("生成中", True)]})
out["wait_att_video_poster_rejected"] = {
    "ret": e._wait_attachment("", 30, "video", base_att_cnt=0), "log": p.log,
    "n_scroll": n_of(p, "scroll"), "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state"),
    "secs": FIXED[0] - 1700000000.0}

# 短超时同样一无所获：静帧不能被当成品。
FIXED[0] = 1700000000.0
e, p = fresh({"atts": [att_img], "state": [st("生成中", True)]})
out["wait_att_video_poster_short"] = {
    "ret": e._wait_attachment("", 10, "video", base_att_cnt=0), "log": p.log,
    "n_scroll": n_of(p, "scroll"), "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state")}

# 下面三条钉的是「静帧之外」的其余分支，poster_rejected/poster_short 覆盖不到它们。

# 上游自己在 a6617a9 加的回归用例，原样复刻：muse 会先把 data-testid 标成
# …-presentation-video，而 <video> 还没挂上，_ATT_JS 的 primary 退回封面 img，
# w/h 还是静帧尺寸。旧写法（"video" in tid）会命中，新写法绝不能命中。
att_tid_cover = json.dumps([{"tid": ATT + "video", "hasVideo": False,
                              "src": "https://x/cover.jpg", "vSrc": "",
                              "iSrc": "https://x/cover.jpg", "w": 1280, "h": 720}])
FIXED[0] = 1700000000.0
e, p = fresh({"atts": [att_tid_cover], "state": [st("生成中", True)]})
out["wait_att_video_tid_cover_rejected"] = {
    "ret": e._wait_attachment("", 8, "video", base_att_cnt=0), "log": p.log,
    "n_scroll": n_of(p, "scroll"), "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state"),
    "secs": FIXED[0] - 1700000000.0}

# 反方向的保险（前面几条全是"必须不命中"，缺一条"必须命中"）：
# 没有 <video>、hasVideo 也是 False、vSrc 也是空，但 src 是显式视频后缀 —— 这算命中。
# 少了这条，Go 把 endswith 那一路判掉（视频永不交付）也一样能过全部反向用例。
att_src_ext = json.dumps([{"tid": ATT + "image", "hasVideo": False,
                           "src": "https://x/a.webm", "vSrc": "",
                           "iSrc": "https://x/a.webm", "w": 0, "h": 0}])
FIXED[0] = 1700000000.0
e, p = fresh({"atts": [att_src_ext], "state": [st("生成中", True)]})
out["wait_att_video_src_ext_only"] = {
    "ret": e._wait_attachment("", 30, "video", base_att_cnt=0), "log": p.log,
    "n_scroll": n_of(p, "scroll"), "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state"),
    "secs": FIXED[0] - 1700000000.0}

# src 里带 "video" 字样、但既不是视频后缀也没有 <video>（muse 把静帧存成 …-video.png 这类）。
# 1.5.x 的 is_match 有 `"video" in src`，会把它当成品；1.6.x 必须全程不命中。
att_src_word = json.dumps([{"tid": ATT + "image", "hasVideo": False,
                            "src": "https://x/video-still.png", "vSrc": "",
                            "iSrc": "https://x/video-still.png", "w": 512, "h": 512}])
FIXED[0] = 1700000000.0
e, p = fresh({"atts": [att_src_word], "state": [st("生成中", True)]})
out["wait_att_video_src_word_only"] = {
    "ret": e._wait_attachment("", 8, "video", base_att_cnt=0), "log": p.log,
    "n_scroll": n_of(p, "scroll"), "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state"),
    "secs": FIXED[0] - 1700000000.0}

# 1.6.0 新增：提示词 30 秒还没送达后端（Still sending）→ 快速失败，不再干等整个超时。
FIXED[0] = 1700000000.0
still = json.dumps({"cnt": 0, "txt": "", "stop": True, "tail": "…Still sending your request…"})
e, p = fresh({"atts": ["[]"], "state": [still]})
try:
    e._wait_attachment("", 60, "image", base_att_cnt=0)
    out["wait_att_still_sending"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["wait_att_still_sending"] = {
        "err": type(exc).__name__ + ":" + str(exc), "log": p.log,
        "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state"),
        "secs_before_fail": FIXED[0] - 1700000000.0}

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
    out["wait_att_textonly"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

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
# 注意消费顺序：基准 (_poll_chat) 在 _send 之前取，再等新气泡出现，再进入流式增量循环。
# reset_thread / _send 都跑真实实现，把它们自己的 CDP 调用也记进日志，Go 才有的回放。
SEND_Q = {"reset_nav": [0], "focus": [1], "rect": ['{"x":5,"y":5}'], "setter": [2],
          "send_btn": ["clicked"]}


def q(**kw):
    d = dict(SEND_Q)
    d.update(kw)
    return d


e, p = fresh(q(state=[st("", False, cnt=0), st("你", True, cnt=1), st("你好", True, cnt=1),
                      st("你好", False, cnt=1), st("你好", False, cnt=1),
                      st("你好", False, cnt=1)]))
e.ensure_page = lambda *a, **k: p
out["chat_stream"] = {"deltas": list(e.chat_stream({"a": "1"}, "hi", timeout=30)), "log": p.log}

e, p = fresh(q(state=[st("", False, cnt=0)], tail=["Connecting..."]))
e.ensure_page = lambda *a, **k: p
try:
    list(e.chat_stream({"a": "1"}, "hi", timeout=30))
    out["chat_stream_connecting"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    # 60ms 采样 × 14s = 几百次轮询，全量日志没有意义；改记「按规则分类的调用次数」，
    # Go 侧用同一个阈值跑出同样的次数才算一致。
    def cnt_of(rule):
        return sum(1 for x in p.log if x[0] == "js" and rule_of(EXPRS[x[1]]) == rule)

    out["chat_stream_connecting"] = {"err": type(exc).__name__ + ":" + str(exc),
                                     "n_state": cnt_of("state"), "n_tail": cnt_of("tail"),
                                     "n_total": len(p.log), "head_log": p.log[:9]}

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
                        "name_len": len(res["filename"]), "name_tail": res["filename"][-4:]}

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
e.extract_bytes = lambda src, expect="image": (b"", "", "")
e._download_fallback = lambda src, timeout=180: None
e._attach_image = lambda img: None
try:
    e.generate({"a": "1"}, "画猫", timeout=60, reference_image="https://x/ref.png")
    out["generate_lost_file"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_lost_file"] = {"err": type(exc).__name__ + ":" + str(exc)}

# ---------------- quota ----------------
e, p = fresh({"quota_settings": ['{"x":10,"y":20}'], "quota_menu": ['{"x":30,"y":40}'], "dialog_txt": [
    "Free plan\nWeekly limit resets on Sep 30\n1% used\nAdditional tokens\nNever expires\n0% used (2B tokens left)"]})
e.ensure_page = lambda *a, **k: p
q = e.quota({"a": "1"})
out["quota"] = {"ret": q, "log": p.log, "sends": [x for x in p.log if x[0] == "send"]}

# Settings 按钮找不到 / 菜单没弹出 / 面板文本一直没有 "used"（走完 6 次轮询后 found=False）
e, p = fresh({"quota_settings": [None]})
e.ensure_page = lambda *a, **k: p
try:
    e.quota({"a": "1"})
    out["quota_no_button"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["quota_no_button"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

e, p = fresh({"quota_settings": ['{"x":10,"y":20}'], "quota_menu": [None]})
e.ensure_page = lambda *a, **k: p
try:
    e.quota({"a": "1"})
    out["quota_no_menu"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["quota_no_menu"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

e, p = fresh({"quota_settings": ['{"x":10,"y":20}'], "quota_menu": ['{"x":30,"y":40}'],
              "dialog_txt": [""]})
e.ensure_page = lambda *a, **k: p
try:
    e.quota({"a": "1"})
    out["quota_no_text"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["quota_no_text"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

# ---------------- _clear_attachments / _attach_image ----------------
REF = "data:image/png;base64,iVBORw0KGgo="

e, p = fresh({"clear": [2]})
e._clear_attachments()
out["clear_attachments"] = {"log": p.log}

e, p = fresh({"clear": [0], "inject": ['{"ok": true}'], "attached": ["remove"]})
e._attach_image(REF)
out["attach_image_ok"] = {"log": p.log}

e, p = fresh({"clear": [0], "inject": ['{"ok": false, "err": "no-file-input"}']})
try:
    e._attach_image(REF)
    out["attach_image_notok"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["attach_image_notok"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

e, p = fresh({"clear": [0], "inject": ['{"ok": true}'], "attached": [""] * 80})
try:
    e._attach_image(REF)
    out["attach_image_unconfirmed"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["attach_image_unconfirmed"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

e, p = fresh({})
try:
    e._attach_image("data:image/png;base64,")
    out["attach_image_nob64"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["attach_image_nob64"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log}

# ---------------- _download_fallback ----------------
for _f in os.listdir(Cfg.download_dir):
    os.remove(os.path.join(Cfg.download_dir, _f))
e, p = fresh({"dl_clicked": ["none"]})
out["dl_none"] = {"ret": e._download_fallback("https://x/a"), "log": p.log}

# 点击后同时落下 3 个新文件：只有「非 .crdownload 里 mtime 最新」的那个能被当结果拿回。
# 打桩 JS 的副作用发生在「第一次 js 调用」，Go 侧 replayPage 用同样的 hook 复现。
open(os.path.join(Cfg.download_dir, "old.png"), "wb").write(b"OLD")
os.utime(os.path.join(Cfg.download_dir, "old.png"), (1600000000, 1600000000))
e, p = fresh({})
_fired = {"done": False}


def dl_js(expr, await_promise=False, timeout=None):
    if not _fired["done"]:
        _fired["done"] = True
        for nm, mt, body in (("new.png", 1700000000, b"NEW"),
                            ("new2.png", 1800000000, b"NEW2BYTES"),
                            ("zz.png.crdownload", 1900000000, b"PART")):
            fp = os.path.join(Cfg.download_dir, nm)
            open(fp, "wb").write(body)
            os.utime(fp, (mt, mt))
    p.log.append(["js", intern(expr), await_promise, "ok", False])
    return "ok"


p.js = dl_js
r = e._download_fallback("https://x/a", timeout=6)
out["dl_ok"] = {"ret": (r or "").replace(Cfg.download_dir, "<DL>"), "log": p.log}

# ---------------- _debug_dump ----------------
e, p = fresh({"dump": ['{"atts":[],"buttons":["发送"],"tail":"hi"}']})
e._debug_dump("unit")
out["debug_dump"] = {"content": open(os.path.join(Cfg.data_dir, "debug-unit.json"),
                                     encoding="utf8").read(), "log": p.log}

e, p = fresh({"dump": [{"__raise__": True}]})
_dp = os.path.join(Cfg.data_dir, "debug-raise.json")
if os.path.exists(_dp):
    os.remove(_dp)
e._debug_dump("raise")
out["debug_dump_raise"] = {"exists": os.path.exists(_dp), "log": p.log}

# ---------------- generate：整条链路的 CDP 序列（只打桩 ensure_page） ----------------
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_img],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "extract": [payload]})
e.ensure_page = lambda *a, **k: p
res = e.generate({"a": "1"}, "画猫", expect="image", timeout=60)
out["generate_full_image"] = {"log": p.log, "via": res["via"], "ext": res["ext"],
                              "kind": res["kind"], "size": res["size"], "mime": res["mime"],
                              "w": res["w"], "h": res["h"], "attachment": res["attachment"],
                              "ret_keys": sorted(res.keys()), "name_len": len(res["filename"]),
                              "name_tail": res["filename"][-4:],
                              "in_media_dir": res["path"].startswith(Cfg.media_dir),
                              "bytes_b64": base64.b64encode(
                                  open(res["path"], "rb").read()).decode()}

# 带参考图：_attach_image 必须发生在「记基线」之前，否则会把旧历史图当基线。
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "inject": ['{"ok": true}'],
              "attached": ["remove"], "atts": ["[]", att_img],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "extract": [payload]})
e.ensure_page = lambda *a, **k: p
res3 = e.generate({"a": "1"}, "画猫", expect="image", timeout=60, reference_image=REF)
out["generate_full_image_ref"] = {"log": p.log, "via": res3["via"], "ext": res3["ext"],
                                  "kind": res3["kind"], "size": res3["size"],
                                  "attachment": res3["attachment"],
                                  "ret_keys": sorted(res3.keys())}

# 页内 fetch 全军覆没 → 走「点这个结果自己的下载按钮」兜底，再从 downloads 里搬文件。
for _f in os.listdir(Cfg.download_dir):
    os.remove(os.path.join(Cfg.download_dir, _f))
SIDE["dl_clicked"] = lambda: (
    open(os.path.join(Cfg.download_dir, "shot.png"), "wb").write(b"PNGBYTES"),
    os.utime(os.path.join(Cfg.download_dir, "shot.png"), (1750000000, 1750000000)))
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_img],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)],
              "extract": [json.dumps({"ok": False, "err": "media-http-404"})],
              "dump": ["{}"], "dl_clicked": ["ok"]})
e.ensure_page = lambda *a, **k: p
res4 = e.generate({"a": "1"}, "画猫", expect="image", timeout=60)
SIDE.pop("dl_clicked", None)
out["generate_full_download_fallback"] = {"log": p.log, "via": res4["via"], "ext": res4["ext"],
                                          "kind": res4["kind"], "size": res4["size"],
                                          "mime": res4["mime"], "w": res4["w"], "h": res4["h"],
                                          "attachment": res4["attachment"],
                                          "ret_keys": sorted(res4.keys()),
                                          "name_len": len(res4["filename"]),
                                          "name_tail": res4["filename"][-4:],
                                          "bytes_b64": base64.b64encode(
                                              open(res4["path"], "rb").read()).decode()}

e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]"],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "dump": ["{}"]})
e.ensure_page = lambda *a, **k: p
_noatt = os.path.join(Cfg.data_dir, "debug-no-attachment.json")
if os.path.exists(_noatt):
    os.remove(_noatt)
try:
    e.generate({"a": "1"}, "画猫", expect="image", timeout=3)
    out["generate_timeout"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_timeout"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log,
                               "dump_file": os.path.exists(_noatt)}

# ---------------- generate：视频分支（整链路，只打桩 ensure_page） ----------------
# 1.6.0 起视频链路有三条硬规矩：只认真实视频源、静帧永不冒充成品、
# 成品类型和期望不符时明确报错（blob 与下载兜底两条路都有守卫）。
mp4_b64 = base64.b64encode(b"\x00\x00\x00\x18ftypmp42mp42isom").decode()
vid_payload = json.dumps({"ok": True, "mime": "video/mp4", "size": 16,
                          "url": "https://x/a.mp4", "b64": mp4_b64})
poster_b64 = base64.b64encode(b"\xff\xd8\xff\xe0jpgdata").decode()
poster_payload = json.dumps({"ok": True, "mime": "image/jpeg", "size": 11,
                             "url": "https://x/poster.jpg", "b64": poster_b64})
raw_b64 = base64.b64encode(b"rawbytes").decode()
unknown_payload = json.dumps({"ok": True, "mime": "application/octet-stream", "size": 9,
                              "url": "https://x/blob", "b64": raw_b64})
att_vid_poster = json.dumps([{"tid": ATT + "image", "hasVideo": False,
                              "src": "https://x/poster.jpg", "vSrc": "",
                              "iSrc": "https://x/poster.jpg", "w": 640, "h": 360}])
# 伪装成视频的附件：tid 是 video、src 带 .mp4 后缀，但真实内容可能仍是图片。
att_vid_src_mp4 = json.dumps([{"tid": ATT + "video", "hasVideo": False,
                               "src": "https://x/a.mp4", "vSrc": "",
                               "iSrc": "", "w": 640, "h": 360}])


def rec(name, res):
    out[name] = {"log": p.log, "via": res["via"], "ext": res["ext"], "kind": res["kind"],
                 "size": res["size"], "mime": res["mime"], "w": res["w"], "h": res["h"],
                 "attachment": res["attachment"], "ret_keys": sorted(res.keys()),
                 "name_len": len(res["filename"]), "name_tail": res["filename"][-4:],
                 "in_media_dir": res["path"].startswith(Cfg.media_dir),
                 "bytes_b64": base64.b64encode(open(res["path"], "rb").read()).decode()}


# 真视频：vSrc 有值 → selected_src 用 vSrc，_pick_ext 由 mime 定 .mp4，kind=video。
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_vid, att_vid],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "extract": [vid_payload]})
e.ensure_page = lambda *a, **k: p
rec("generate_full_video", e.generate({"a": "1"}, "红球弹起", expect="video", timeout=60))

# mime/URL 都认不出扩展名时才轮到 expect 兜底：video → .mp4，kind 也跟着变 video。
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_vid, att_vid],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "extract": [unknown_payload]})
e.ensure_page = lambda *a, **k: p
rec("generate_full_video_ext_from_expect",
    e.generate({"a": "1"}, "红球弹起", expect="video", timeout=60))

# 只有封面静帧：严格匹配下视频候选为空 —— 一路等到超时，报「未出现新的生成结果」。
FIXED[0] = 1700000000.0
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_vid_poster],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "dump": ["{}"]})
e.ensure_page = lambda *a, **k: p
try:
    e.generate({"a": "1"}, "红球弹起", expect="video", timeout=60)
    out["generate_full_video_poster_rejected"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_full_video_poster_rejected"] = {
        "err": type(exc).__name__ + ":" + str(exc), "log": p.log,
        "n_atts": n_of(p, "atts"), "n_state": n_of(p, "state"),
        "secs": FIXED[0] - 1700000000.0}

# 10s 短超时：同样是「等待生成超时」，只是轮询少。
FIXED[0] = 1700000000.0
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_vid_poster],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "dump": ["{}"]})
e.ensure_page = lambda *a, **k: p
try:
    e.generate({"a": "1"}, "红球弹起", expect="video", timeout=10)
    out["generate_full_video_gives_up"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_full_video_gives_up"] = {"err": type(exc).__name__ + ":" + str(exc),
                                           "log": p.log, "n_atts": n_of(p, "atts"),
                                           "n_state": n_of(p, "state")}

# 提取回来的成品是图片（mime 判成 .jpg）而期望视频：拒绝交付，明确报错。
FIXED[0] = 1700000000.0
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_vid_src_mp4],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)],
              "extract": [poster_payload]})
e.ensure_page = lambda *a, **k: p
try:
    e.generate({"a": "1"}, "红球弹起", expect="video", timeout=60)
    out["generate_full_video_image_guard_blob"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_full_video_image_guard_blob"] = {
        "err": type(exc).__name__ + ":" + str(exc), "log": p.log}

# 下载兜底拿回 .jpg 也拒绝交付。
for _f in os.listdir(Cfg.download_dir):
    os.remove(os.path.join(Cfg.download_dir, _f))
SIDE["dl_clicked"] = lambda: (
    open(os.path.join(Cfg.download_dir, "shot.jpg"), "wb").write(b"JPGDATA"),
    os.utime(os.path.join(Cfg.download_dir, "shot.jpg"), (1750000000, 1750000000)))
FIXED[0] = 1700000000.0
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]", att_vid_src_mp4],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)],
              "extract": [json.dumps({"ok": False, "err": "media-http-404"})],
              "dl_clicked": ["ok"]})
e.ensure_page = lambda *a, **k: p
try:
    e.generate({"a": "1"}, "红球弹起", expect="video", timeout=60)
    out["generate_full_video_image_guard_download"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_full_video_image_guard_download"] = {
        "err": type(exc).__name__ + ":" + str(exc), "log": p.log}
SIDE.pop("dl_clicked", None)

# b19b484 把时限改成 monotonic 绝对 deadline，并在**发送之前**加了一道守卫：
# 准备阶段（滚动/清附件/取基线/数气泡）一旦把时限吃光，就一个字都不发。
# timeout=0 时 FakeTime 与 deadline 相等，守卫按 `>=` 判定，因此必然命中；
# n_send_btn 记成 0 就是"守卫在 _send 之前"的证据 —— 挪到后面去就会红。
FIXED[0] = 1700000000.0
e, p = fresh({"reset_nav": [0], "scroll": [1], "clear": [0], "atts": ["[]"],
              "agent_cnt": ["0"], "focus": [1], "rect": ['{"x":10,"y":20}'], "setter": [3],
              "send_btn": ["clicked"], "state": [st("生成中", True)], "dump": ["{}"]})
e.ensure_page = lambda *a, **k: p
try:
    e.generate({"a": "1"}, "红球弹起", expect="video", timeout=0)
    out["generate_prep_timeout"] = {"err": None}
except Exception as exc:  # noqa: BLE001
    out["generate_prep_timeout"] = {"err": type(exc).__name__ + ":" + str(exc), "log": p.log,
                                    "n_send_btn": n_of(p, "send_btn"),
                                    "n_atts": n_of(p, "atts"), "n_scroll": n_of(p, "scroll")}

# 各轮询场景的超时是调用方传进来的，Go 必须用同一个值，否则循环次数不可能一致。
for _k, _t in (("wait_att_immediate", 30), ("wait_att_video_stable", 30),
               ("wait_att_video_poster_rejected", 30), ("wait_att_video_poster_short", 10),
               ("wait_att_video_tid_cover_rejected", 8), ("wait_att_video_src_ext_only", 30),
               ("wait_att_video_src_word_only", 8), ("wait_att_still_sending", 60),
               ("wait_att_credits", 30), ("wait_att_textonly", 60),
               ("wait_att_media_report", 30), ("wait_att_baseline_excluded", 3),
               ("chat_stream", 30), ("chat_stream_connecting", 30),
               ("generate_full_image", 60), ("generate_full_image_ref", 60),
               ("generate_full_download_fallback", 60),
               ("generate_full_video", 60), ("generate_full_video_ext_from_expect", 60),
               ("generate_full_video_poster_rejected", 60), ("generate_full_video_gives_up", 10),
               ("generate_prep_timeout", 0),
               ("generate_full_video_image_guard_blob", 60),
               ("generate_full_video_image_guard_download", 60),
               ("generate_timeout", 3),
               ("dl_ok", 6)):
    if isinstance(out.get(_k), dict):
        out[_k]["timeout"] = _t

fb = {}
for pg in ALL_PAGES:
    for r in pg.fallbacks:
        fb[r] = fb.get(r, 0) + 1
out["__fallbacks__"] = fb

print("@@JSON@@")
print(base64.b64encode(json.dumps({"sections": out, "exprs": EXPRS}, ensure_ascii=False)
                       .encode("utf-8")).decode())
