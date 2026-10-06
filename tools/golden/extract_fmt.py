"""对 engine.py 里每条含 %s 的 JS 模板，用真 Python 做一次 % 格式化，输出原文与结果。

Go 侧用自己的 pyFormat 跑同样输入，比对结果 —— 这样 %s/%% 语义就不是我口头断言。
"""
import ast
import base64
import json
import sys

SRC = sys.argv[1] if len(sys.argv) > 1 else "/app/engine.py"
MARK = ("function(", "document.", "=>", "querySelector", "window.", "JSON.stringify")
tree = ast.parse(open(SRC, encoding="utf-8").read())

rows = []
for node in ast.walk(tree):
    if not (isinstance(node, ast.Constant) and isinstance(node.value, str)):
        continue
    s = node.value
    if len(s) <= 25 or not any(m in s for m in MARK):
        continue
    n = s.count("%s")
    if n == 0:
        continue
    args = tuple("ARG%d" % (i + 1) for i in range(n))
    try:
        out = s % args
    except Exception as exc:  # noqa: BLE001
        out = "ERROR:" + type(exc).__name__ + ":" + str(exc)
    b = lambda x: base64.b64encode(x.encode("utf-8")).decode()  # noqa: E731
    rows.append({"line": node.lineno, "n": n, "tmpl": b(s), "formatted": b(out)})

print("=== FMT %d ===" % len(rows))
for r in rows:
    print(json.dumps(r))
