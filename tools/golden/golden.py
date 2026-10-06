"""在 NAS 容器里用真 store.py 生成金样本（固定 uuid/时间，可复现）。"""
import json
import sys

sys.path.insert(0, "/app")
import store as S  # noqa: E402

FIXED_TIME = 1700000000.5

state = {"n": 0, "t": FIXED_TIME}


class FakeUUID:
    @property
    def hex(self):
        return "%d%s" % (state["n"], "0" * 63)


def fake_uuid4():
    state["n"] += 1
    return FakeUUID()


class FakeTime:
    @staticmethod
    def time():
        return state["t"]

    @staticmethod
    def sleep(*a):
        pass


S.uuid.uuid4 = fake_uuid4
S.time = FakeTime


class Cfg:
    def __init__(self, d):
        self.d = d

    @property
    def accounts_file(self):
        return self.d + "/accounts.json"

    @property
    def tasks_file(self):
        return self.d + "/tasks.json"


import os  # noqa: E402

D = "/tmp/golden"
os.makedirs(D, exist_ok=True)
s = S.Store(Cfg(D))

a1 = s.add_account(
    {"hatch_sess": "s1", "hatch_gw": "g1", "hatch_vml": "v1",
     "hatch_native_auth_device": "d1", "extra": "<a&b> 中文"},
    "测试 <A&B> 周杰伦",
    {"hatch_vml": "1700086400", "hatch_gw": 1790000000, "bad": "abc", "zero": 0})
a2 = s.add_account({"hatch_sess": "s2"}, "", {"hatch_sess": 1700090000})
s.mark(a1["id"], True, "会话有效")
s.touch_keepalive(a2["id"], None, "保活未确认（保留账号状态）: HTTP 503")
s.update_account(a1["id"],
                 quota={"plan": "Free plan", "weekly_used_pct": 12},
                 cookies_exp={"hatch_vml": 1700172800})
picked = s.pick_account(preferred_id=a1["id"], rotate=True)
t1 = s.create_task("image", "画一只猫")
s.update_task(t1["id"], status="completed",
              result={"url": "/v1/media/x.webp", "kind": "image"})

print("=== PICKED=%s" % (picked["id"] == a1["id"]))
print("=== STATS=%s" % json.dumps(s.stats(), sort_keys=True, ensure_ascii=False))
print("=== ACCOUNTS_B64===")
print(__import__("base64").b64encode(open(s.cfg.accounts_file, "rb").read()).decode())
print("=== TASKS_B64===")
print(__import__("base64").b64encode(open(s.cfg.tasks_file, "rb").read()).decode())
