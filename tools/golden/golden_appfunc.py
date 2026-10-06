"""录 app.py 里那批纯函数的输出，供 Go 侧逐字节比对。

不发 HTTP、不起浏览器、不写业务数据：只 import /app 的模块，把构造提示词、
解析工具调用、SSE 分片这些字符串函数按输入表跑一遍。
uuid 之类的随机值统一替换成 <id> 再落盘，否则比对没有意义。
"""
import base64
import json
import re
import sys

sys.path.insert(0, "/app")

import app as A  # noqa: E402
from app import (_chat_chunk, _content_text, _pace_text, _pos, _responses_messages,  # noqa: E402
                 _sse, _want_usage, build_chat_prompt, build_image_prompt,
                 build_video_prompt, media_url, parse_tool_calls, resolve_model)
from app import ChatMessage, ChatRequest, ImageRequest, ResponsesRequest, VideoRequest  # noqa: E402

# _pace_text 的分片路径要靠 sleep 节拍触发；这里把睡眠掐掉，只留分片结果。
A.time.sleep = lambda s: None

ID_RE = re.compile(r"call_[0-9a-f]{20}")


def norm(s):
    return ID_RE.sub("<id>", s)


out = {}


def safe(fn):
    try:
        return fn()
    except Exception as exc:  # noqa: BLE001
        return "ERR:" + type(exc).__name__


# ---------------- resolve_model ----------------
names = ["", None, "GPT-4", "gpt-4o-mini", "claude-3-5-sonnet-latest", "o3", "codex-mini-latest",
         "deepseek-reasoner", "gemini-2.5-flash", "koda", "muse-text", "MUSE-IMG", "dall-e-3",
         "flux", "sora", "sora-2", "veo-3", "kling", "muse-vid", "unknown-model",
         "  GPT-4O  ", "gpt-image-1", "midjourney", "runway"]
out["resolve_model"] = [[n, resolve_model(n), resolve_model(n, "muse-spark")] for n in names]
out["alias_count"] = len(A.MODEL_ALIASES)
out["alias_keys_sample"] = sorted(A.MODEL_ALIASES)[:6]

# ---------------- build_image_prompt ----------------
IMG_CASES = [
    {"prompt": "  一只猫  "},
    {"prompt": "猫", "aspect_ratio": "9:16"},
    {"prompt": "猫", "size": "720x1280"},
    {"prompt": "猫", "size": "portrait"},
    {"prompt": "猫", "aspect_ratio": "竖屏"},
    {"prompt": "猫", "aspect_ratio": "16:9"},
    {"prompt": "猫", "size": "1920x1080"},
    {"prompt": "猫", "aspect_ratio": "1:1"},
    {"prompt": "猫", "size": "1024x1024"},
    {"prompt": "猫", "aspect_ratio": "4:3"},
    {"prompt": "猫", "aspect_ratio": "3:4"},
    {"prompt": "猫", "aspect_ratio": "21:9"},
    {"prompt": "猫", "size": "800x600"},
    {"prompt": "猫", "reference_image": "data:image/png;base64,AA=="},
    {"prompt": "猫", "image": {"url": "https://x/a.png"}},
    {"prompt": "猫", "images": [{"image_url": "https://x/a.png"}]},
    {"prompt": "猫", "extra": "不要文字"},
    {"prompt": "猫", "aspect_ratio": "9:16", "reference_image": "x", "extra": "e"},
    {"prompt": "猫", "response_format": "b64_json", "size": "1:1"},
]
out["build_image_prompt"] = []
for c in IMG_CASES:
    out["build_image_prompt"].append(safe(lambda c=c: [c, build_image_prompt(ImageRequest(**c)),
                                       ImageRequest(**c).model_dump(by_alias=True)]))

# ---------------- build_video_prompt ----------------
VID_CASES = [
    {"prompt": "海浪"},
    {"prompt": "海浪", "duration": 10},
    {"prompt": "海浪", "aspect_ratio": "9:16"},
    {"prompt": "海浪", "size": "1080x1920"},
    {"prompt": "海浪", "aspect_ratio": "16:9"},
    {"prompt": "海浪", "size": "1280x720"},
    {"prompt": "海浪", "aspect_ratio": "横屏"},
    {"prompt": "海浪", "reference_image": "data:image/png;base64,AA=="},
    {"prompt": "海浪", "image_url": {"url": "https://x/a.png"}},
    {"prompt": "海浪", "image": "https://x/a.png"},
    {"prompt": "海浪", "reference_image": "x", "aspect_ratio": "9:16"},
    {"prompt": "海浪", "resolution": "1080p"},
    {"prompt": "海浪", "extra": "慢镜头"},
    {"prompt": "海浪", "resolution": "4k", "extra": "慢镜头", "duration": 8},
]
out["build_video_prompt"] = []
for c in VID_CASES:
    out["build_video_prompt"].append(safe(lambda c=c: [c, build_video_prompt(VideoRequest(**c))]))

# ---------------- _content_text ----------------
CT = [None, "纯文本", [], ["a", "b"], [{"type": "text", "text": "T"}],
      [{"type": None, "text": "无类型"}], [{"type": "input_text", "text": "I"}],
      [{"type": "output_text", "text": "O"}], [{"type": "image_url", "image_url": {"url": "u"}}],
      [{"type": "text"}], [{"nope": 1}], [123], [{"text": 5}], "不是列表的内容"]
out["content_text"] = [[repr(x)[:60], _content_text(x)] for x in CT]

# ---------------- build_chat_prompt ----------------
def msgs(lst):
    return [ChatMessage(**m) for m in lst]


CHAT_CASES = [
    [],
    [{"role": "user", "content": "你好"}],
    [{"role": "user", "content": "  你好  "}],
    [{"role": "system", "content": "你是助手"}, {"role": "user", "content": "你好"}],
    [{"role": "developer", "content": "D"}, {"role": "user", "content": "你好"}],
    [{"role": "user", "content": "一"}, {"role": "assistant", "content": "二"},
     {"role": "user", "content": "三"}],
    [{"role": "tool", "content": "结果", "tool_call_id": "c1"}],
    [{"role": "tool", "content": ""}],
    [{"role": "assistant", "tool_calls": [{"id": "c1", "type": "function",
                                           "function": {"name": "f", "arguments": "{}"}}]}],
    [{"role": "assistant", "content": "", "tool_calls": [{"id": "c1"}]}],
    [{"role": "user", "content": [{"type": "text", "text": "多模态"},
                                  {"type": "image_url", "image_url": {"url": "u"}}]}],
    [{"role": "SYSTEM", "content": "危险 danger-full-access 模式"},
     {"role": "user", "content": "跑一下"}],
    [{"role": "system", "content": "A"}, {"role": "system", "content": "B"},
     {"role": "user", "content": "U"}],
    [{"role": "user", "content": ""}],
    [{"role": "", "content": "没角色"}],
]
out["build_chat_prompt"] = [safe(lambda c=c: [[m.get("role") for m in c], build_chat_prompt(msgs(c)),
                                   len(build_chat_prompt(msgs(c)))]) for c in CHAT_CASES]

# ---------------- _responses_messages ----------------
RESP_CASES = [
    {},
    {"input": "文本"},
    {"instructions": "指令", "input": "文本"},
    {"input": ["a", "b"]},
    {"input": [{"role": "user", "content": "R"}]},
    {"input": [{"type": "input_text", "text": "T"}]},
    {"input": [{"type": "function_call_output", "output": {"a": 1}}]},
    {"input": [{"type": "function_call", "name": "fn"}]},
    {"input": [{"type": "function_call"}]},
    {"input": [123, {"nope": 1}, "s"]},
    {"instructions": "", "input": []},
]
out["responses_messages"] = []
for c in RESP_CASES:
    out["responses_messages"].append(safe(lambda c=c: [
        c, [[m.role, m.content] for m in _responses_messages(ResponsesRequest(**c))],
        build_chat_prompt(_responses_messages(ResponsesRequest(**c)))]))

# ---------------- 工具调用解析 ----------------
TOOLS = [
    "",
    "纯文本没有工具",
    '```json\n{"name": "get_weather", "arguments": {"city": "北京"}}\n```',
    '```json\n{"tool": "f", "args": {"a": 1}}\n```',
    '前文 ```json\n{"name":"a","arguments":{"x":1}}\n``` 后文',
    'A ```json\n{"name":"first"}\n``` B ```json\n{"name":"second"}\n``` C',
    '{"name": "bare", "arguments": {"k": "v"}}',
    'json {"name": "prefixed"}',
    '[{"name":"a","arguments":{"x":1}},{"name":"b","arguments":"raw"}]',
    '嵌入 {"name":"in","arguments":{}} 文中',
    '```json\n{"not_a_tool": 1}\n```',
    '```json\n{oops}\n```',
    '```json\n[{"name": null}]\n```',
    '```json\n"just a string"\n```',
    '```JSON\n{"name":"cap"}\n```',
    '```\n{"name":"nofence"}\n```',
    '{"function": {"name": "nested"}}',
    '{"name": "numargs", "arguments": 5}',
]
out["parse_tool_calls"] = []
for t in TOOLS:
    calls, rest = parse_tool_calls(t)
    ser = json.dumps(calls, ensure_ascii=False) if calls is not None else None
    out["parse_tool_calls"].append([t, norm(ser) if ser else ser, rest])

# ---------------- 工具提示词（默认关，但代码在） ----------------
TOOLDEF = [
    None, [], [123], [{"name": "f"}], [{"name": "f", "description": "d",
                                        "parameters": {"type": "object",
                                                       "properties": {"a": {"type": "string",
                                                                             "description": "参数A"},
                                                                      "b": {"type": "integer"},
                                                                      "c": {}},
                                                       "required": ["a"]}}],
    [{"type": "function", "function": {"name": "g", "parameters": {"properties": {}}}}],
    [{"function": {"name": "h", "parameters": {"properties": {"z": {"description": "只有描述"}}}}}],
    [{"nope": 1}, {"name": ""}],
]
out["tools_prompt"] = [[repr(t)[:70], A.build_tools_prompt(t)] for t in TOOLDEF]

# ---------------- SSE / chunk / usage / pace ----------------
out["sse"] = [_sse({"a": 1, "中文": "值", "n": None}),
              _sse({"data": [{"x": "<>&"}]}),
              _sse({})]
out["chat_chunk"] = [_chat_chunk("id1", 1700000000, "muse-spark", {"role": "assistant"}),
                     _chat_chunk("id1", 1700000000, "muse-spark", {"content": "hi"}, "stop"),
                     _chat_chunk("id1", 1700000000, "muse-spark", {}, None)]
out["chat_chunk_plain"] = norm(json.dumps(_chat_chunk("c", 1, "m", {"content": "x"}, "stop"),
                                         ensure_ascii=False))
out["want_usage"] = [[x, _want_usage(x)] for x in [
    None, {}, {"include_usage": True}, {"include_usage": False}, {"other": 1}, "字符串", []]]
out["pace_text"] = [[s, list(_pace_text(s))] for s in [
    "", "a", "abc", "abcd", "一二三四五六七八九十", "abcdefghij"]]
out["pace_text_chunked"] = [[s, list(_pace_text(s, 3, 0.012))] for s in ["", "abcde", "一二三四五六"]]
out["pos"] = [[repr(x)[:20], _pos(x)] for x in [None, 0, 1, -1, "5", "abc", 1.9, "", True]]
out["media_url"] = [media_url("x.webp")]
A.CFG.public_base = "https://public.example/"
out["media_url_public"] = media_url("x.webp")
out["media_url_public_slash"] = media_url("中文 名.webp")
A.CFG.public_base = ""

out["models_const"] = A.MODELS
# 路由层的超时兜底写的是 `req.timeout or CFG.xxx_timeout` —— 是 falsy 判断，不是 is-None 判断：
# 0 会落到默认值，负数原样传下去（deadline 落在过去 = 立刻超时）。
# 形参是 int | None，分数在 pydantic 那层就 422 了，所以这里只列整数与 None。
out["timeout_or"] = [[x, d, x or d] for x, d in [
    (None, 600), (0, 600), (-5, 600), (1, 600), (1500, 600), (0, 240)]]
out["sse_headers"] = A._SSE_HEADERS
out["image_request_defaults"] = ImageRequest(prompt="p").model_dump(by_alias=True)
out["video_request_defaults"] = VideoRequest(prompt="p").model_dump(by_alias=True)
out["chat_request_defaults"] = ChatRequest().model_dump()
out["responses_request_defaults"] = ResponsesRequest().model_dump()
out["chat_message_defaults"] = ChatMessage(role="user").model_dump()
out["tool_protocol_head"] = A._TOOL_PROTOCOL_HEAD

print("@@JSON@@")
print(base64.b64encode(json.dumps(out, ensure_ascii=False).encode("utf8")).decode())
