"""从部署中的 engine.py 抽出所有注入用 JS 文本，逐条 base64 输出。

三类条目：
  const   —— 独立的字符串字面量（含函数内联的）
  concat  —— 由字面量 + 变量拼出来的模板，变量位置用 \x00VAR:名字\x00 占位，
             这样 Go 侧的拼装结果可以逐字节还原并比对
  named   —— 指定的模块级具名常量（ATT_SEL 这类不含 JS 关键词、上面抓不到的）
"""
import ast
import base64
import json
import sys

SRC = sys.argv[1] if len(sys.argv) > 1 else "/app/engine.py"
NAMED = sys.argv[2:] or ["ATT_SEL", "ESSENTIAL_COOKIES"]

tree = ast.parse(open(SRC, encoding="utf-8").read())
MARK = ("function(", "document.", "=>", "querySelector", "window.", "JSON.stringify")


def looks_like_js(s):
    return isinstance(s, str) and len(s) > 25 and any(m in s for m in MARK)


def b64(s):
    return base64.b64encode(s.encode("utf-8")).decode()


consts, concats = [], []
for node in ast.walk(tree):
    if isinstance(node, ast.Constant) and looks_like_js(node.value):
        consts.append({"line": node.lineno, "b64": b64(node.value)})
    elif isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        buf = []

        def flat(n):
            if isinstance(n, ast.BinOp) and isinstance(n.op, ast.Add):
                flat(n.left)
                flat(n.right)
            elif isinstance(n, ast.Constant) and isinstance(n.value, str):
                buf.append(n.value)
            elif isinstance(n, (ast.Name, ast.Attribute)):
                nm = n.id if isinstance(n, ast.Name) else n.attr
                buf.append("\x00VAR:" + nm + "\x00")

        flat(node)
        text = "".join(buf)
        has_var = any(p.startswith("\x00VAR:") for p in buf)
        if has_var and len(text) > 60:
            concats.append({"line": node.lineno, "b64": b64(text)})

named = []
mod = {}
for t in tree.body:
    if isinstance(t, ast.Assign):
        for tgt in t.targets:
            if isinstance(tgt, ast.Name):
                mod[tgt.id] = t.value
for name in NAMED:
    v = mod.get(name)
    if v is None:
        continue
    if isinstance(v, ast.Constant):
        named.append({"name": name, "b64": b64(v.value)})
    elif isinstance(v, (ast.Tuple, ast.List)):
        vals = [e.value for e in v.elts if isinstance(e, ast.Constant)]
        named.append({"name": name, "list": vals})

print("=== JS_CONSTS %d ===" % len(consts))
for c in consts:
    print(json.dumps(dict(c, kind="const")))
print("=== CONCAT_TEMPLATES %d ===" % len(concats))
for c in concats:
    print(json.dumps(dict(c, kind="concat")))
print("=== NAMED %d ===" % len(named))
for n in named:
    print(json.dumps(n))
