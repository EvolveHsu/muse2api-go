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
| `golden_routes.py` | `internal/server/testdata/routes_golden.json` + `openapi.python.json`（43 个路由的对外契约） |

`golden_routes.py` 是打到**现役实例**的，所以只发两类请求：纯读 GET，以及一定在进 handler
之前就被拒的（错 token / 缺必填字段 / 空 prompt 的 400）。handler 一次都不会执行，
因此不起浏览器、不花额度、不改账号库。写接口一律只用坏 token 探 401。
密钥字段（cookies / api_key / cookie_header …）在离开容器前就 REDACT；
list 型接口只记「类型 + 键集合」，免得把线上状态写进金样本。

Go 侧回放时的比对口径（金样本自己也是按这个口径录的）：

- `record=full`：状态码 + content-type + 脱敏后的 body 深比较。易变值按用例忽略
  （`healthz.time`、`readyz.browser_running` 这类线上态）。
- `record=shape`：**只比键集合与类型，不比值**（账号池/任务/媒体是活的线上数据）。
- `record=blob`：静态 HTML（`/`、`/admin`、`/docs`）比 sha256；扩展 zip 只比状态码、
  content-type 与 content-disposition（zip 里带文件时间戳，sha 天生不稳）。
- `record=openapi`：31KB 的 `/openapi.json` 落到 `openapi.python.json`，
  Go 直接内嵌该文件；金样本里留 sha256 + 每个 path 的 method 列表，用作路由表对账。
- header 只比 CORS 相关子集；`date`/`content-length`/`server`/`connection` 不参与。
- 静态文件（cookie-helper 脚本）的 `etag`/`last-modified` 由各自实现生成，不比。

一条命令跑完（base64 送进容器 → AST 预检 → 执行 → 解 base64 落盘 → 有 `MISSING` 就非零退出）：

```
NAS_PW=*** node tools/golden/run_on_nas.mjs golden_generate internal/engine/testdata/generate_golden.json
```

配套的两个本地小工具（不碰 NAS）：

- `check_rules.mjs`：把 `RULES` 顺序套回 `engine_js.json` 里每条真实 JS，打印「哪条规则认领了它」。
  认领不到 = 那段代码不可能被驱动；认领错 = 场景值喂给了另一个函数。
- `show_log.mjs <section> [起始行]`：把金样本某一段的调用序列按规则名展开，用来肉眼核对轮询顺序。

录制时的几条硬规矩（都是踩过之后写下来的）：

1. `RULES` 标记必须取自**真实 JS 文本子串**，写错会静默落到默认值、目标分支根本没被驱动
   （脚本里的 `__fallbacks__` 字段就是用来暴露这种情况的；`MISSING:` 前缀 = 连默认值都没有）。
2. 记日志要在算值**之前**，否则"没配值"的调用会被被测代码的 `except` 吞掉，日志永久缺一条。
3. 假时钟 `FIXED = [1700000000.0]` 是 float64 累加：`0.6` 加 5 次得 `2.9999999999999996`，
   于是「3 秒超时」Python 会多轮一轮。Go 侧测试时钟 `vclock` 同样按 float64 秒累加来对齐这个相位，
   两边的轮询次数才可比 —— 用整纳秒时钟会永远少一轮。
4. 轮询次数由**调用方传进来的 timeout** 决定，所以每个循环场景都要把 timeout 记进金样本，
   Go 从金样本读，不许两边各写各的。
5. 要在「某条 JS 返回之后」往磁盘造文件的场景（下载兜底）用 `SIDE[规则名]` 注册副作用，
   Go 侧用 `replayPage.afterJS` 在同一个时刻复现；`p.js = 覆盖函数`那种写法只适合整段替换。
6. `60ms × 14s` 这类同构轮询不记全量日志（几百条没有信息量），改记 `n_state / n_tail / n_total`
   次数 + `head_log` 前几条严格比对，Go 用 `replayPage.overflow` 进计数模式。
