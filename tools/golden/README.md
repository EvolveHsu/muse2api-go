# 金样本采集脚本（在 NAS 容器里跑，对着**部署中的** Python 源码）

这些脚本不产生任何线上副作用：只 monkeypatch `subprocess.Popen` / `CDP` / `sys.modules["requests"]`，
把"真 engine.py / store.py 打算发出的东西"和纯函数结果录成 JSON，供 Go 侧回放比对。

| 脚本 | 产出 |
|---|---|
| `extract_js.py` | `internal/engine/testdata/engine_js.json`（AST 抽出全部注入 JS，拼接模板带 \0VAR:名字\0 占位） |
| `extract_fmt.py` | `internal/engine/testdata/engine_js_fmt.jsonl`（真 Python `%` 格式化结果） |
| `golden.py` | `internal/store/testdata/*.python.json`（真 store.py 的落盘字节） |
| `golden_engine.py` | `internal/engine/testdata/engine_golden.json`（argv、cookie 注入、开页序列、续签 8 分支、纯函数） |
| `golden_generate.py` | `internal/engine/testdata/generate_golden.json`（生成/对话流程的 CDP 调用序列） |

跑法（`tools/golden/命令抓取NN.txt` 里是 base64 打包 + `docker cp` + 执行的样板）：

```
docker cp tools/golden/golden_engine.py muse2api:/tmp/g.py
docker exec muse2api python3 /tmp/g.py   # 输出 @@JSON@@ + base64
```

注意：`golden_generate.py` 里假 page 的 `RULES` 标记必须取自**真实 JS 文本子串**，
写错会静默落到默认值、导致目标分支根本没被驱动（本项目踩过两次，脚本里的 `__fallbacks__`
字段就是专门用来暴露这种情况的：某规则次数异常偏高即为警讯）。
