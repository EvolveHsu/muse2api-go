# muse2api-go

**[muse2api](https://github.com/czg86389-hub/muse2api)（Python / FastAPI）的 Go 移植**：把 muse.ai 的网页端包装成 OpenAI 兼容接口 —— 对话、生图、图像编辑、生视频，外加一套管理面板（账号 cookie 池、任务、媒体库、额度、保活）。

目标是**逻辑一致、只换语言**：路由、状态码、错误壳、SSE 分帧、响应键序、落盘格式都按上游逐条对齐，而不是重新设计一套。已知差异全部登记在[与上游的差异](#与上游的差异)里。

---

## 上游与许可

| | |
|---|---|
| 原始项目 | https://github.com/czg86389-hub/muse2api |
| 上游许可 | MIT（`LICENSE` 是上游原文照抄，见 `NOTICE` 的派生说明） |
| 移植基线 | 上游 `2374061`（1.5.3）起步，已同步到 `42ee039`（1.5.4） |
| 本仓库 | Go 移植版，不含上游 Python 源码 |

移植方式刻意避免"照着读懂再手写"：注入浏览器的 JavaScript 由 `tools/gen_js.mjs` 从部署中的 `engine.py` 用 AST **程序化抽取**成 `internal/engine/js.go`；管理面板 HTML、`/openapi.json`、`oauth2-redirect.html` 是从线上抓的**字节快照**（`internal/webui/`）。这些东西不是重写的，所以不会"看着差不多但行为差一点"。

---

## 为什么还需要 Chromium

因为它不是 API 客户端，而是**网页自动化**：所有请求最终都由无头 Chromium 通过 CDP 在 muse.ai 页面上操作 DOM 完成。

我们实测过容器内存构成（同一台机器、同一时刻）：523MB 常驻里 Python 应用层只有 **57MB**，其余约 465MB 全是 Chromium 进程树。所以换语言并不能把内存压下去 —— 真正有效的是**空闲释放浏览器**，这也是本移植版的头号行为差异。

早期还做过一次协议嗅探（只读 CDP 会话挂在热标签页上录 WS 报文），结论是浏览器**去不掉**：结果字节是以约 22KB 的分片从一条 `wss://…/v1/noise` 的加密隧道里流回来的，票据由页面内 JS 生成（ES256 JWT + ed25519 公钥参与的 notary 准入），没有任何 REST 端点可以替代。想做无浏览器客户端等于要从压缩后的 JS 里复刻密钥交换与隧道内私有协议，且对方每次发版都可能变。

---

## 接口范围

`openapi.json` 快照里共 **46 个 operation**：`/v1/*` 12 个、`/admin/*` 31 个、其余 3 个（`/`、`/docs`、`/redoc` 等）。

```bash
# 对话
curl -X POST http://<host>:<port>/v1/chat/completions \
  -H "Authorization: Bearer $MUSE2API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"muse-spark","messages":[{"role":"user","content":"你好"}],"stream":true}'

# 生图（同步返回；带 "async":true 走任务轮询）
curl -X POST http://<host>:<port>/v1/images/generations \
  -H "Authorization: Bearer $MUSE2API_KEY" -H "Content-Type: application/json" \
  -d '{"prompt":"一只戴帽子的猫","size":"1024x1024"}'

# 生视频（异步任务，用 GET /v1/videos/{task_id} 轮询）
curl -X POST http://<host>:<port>/v1/videos/generations \
  -H "Authorization: Bearer $MUSE2API_KEY" -H "Content-Type: application/json" \
  -d '{"prompt":"红球在白色桌面上弹起一次","duration":6,"async":true}'
```

> `duration` / `size` / `aspect_ratio` 这些参数是**拼进提示词**交给 muse.ai 的（例如 `时长严格为 2 秒`），真正的产出由对方模型决定。生成视频时如果 muse.ai 只渲染出封面静帧，本服务会照上游语义返回 `kind:"image"` 的 `.jpg` —— 需要必须出片的客户端应按 `kind` 判失败。

---

## 快速开始

### 1. 编译二进制（镜像只拷产物，构建机上不需要装 Docker）

```bash
# 需要 Go ≥ 1.27；CGO_ENABLED=0 是必须的：静态链接后运行时不依赖 glibc 之外的东西
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o build/muse2api-go ./cmd/muse2api
```

### 2. 出镜像并起容器

镜像基于 `debian:bookworm-slim` + 发行版自带的 `chromium`（**与上游同一发行版、同一浏览器包**，否则 DOM 行为不可比）。`deploy/docker-compose.go.yml` 是并行双跑用的样例：独立数据目录 `./data-go`、`init: true`、`shm_size: 2gb`，宿主端口自选（示例里我们用 38467，上游 Python 占 38465）。

```bash
docker build -t muse2api-go:1.5.4 .
docker run -d --name muse2api-go --restart unless-stopped --init \
  -p 8080:18610 -v $PWD/data-go:/app/data --shm-size=2gb \
  -e MUSE2API_KEY=... muse2api-go:1.5.4
```

### 3. 导入账号（cookie）

上游用 `data/accounts.json` 存 cookie 池，两边格式完全相同，所以可以直接把现有账号搬过来：

```bash
docker cp <python容器>:/app/data/accounts.json ./data-go/accounts.json
docker restart muse2api-go
```

然后在 `http://<host>:8080/admin` 面板里看账号状态，或打一次 `/admin/accounts/{id}/keepalive` 验证续期。

### 4. 自检

```bash
curl -s localhost:8080/healthz    # {"status":"ok","time":...}
curl -s localhost:8080/readyz
curl -s -H "Authorization: Bearer $MUSE2API_KEY" localhost:8080/v1/models
```

---

## 配置

环境变量与上游同名同语义，下面是本移植版**新增**或最常碰的几条（完整清单见 `internal/config/config.go`，共 21 个）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `MUSE2API_KEY` | 空则首次启动自动生成并写入 `data/.env` | 对外鉴权密钥 |
| `MUSE2API_BROWSER_IDLE_MINUTES` | `30` | **Go 版新增**：浏览器静默多久后释放。设 `0` 关掉这条 |
| `MUSE2API_WARM_TAB` | `0` | **Go 版新增**：`1` 恢复上游"常驻一个热标签页"的行为（内存换冷启动） |
| `MUSE2API_ACCESS_LOG` | `1` | **Go 版新增**：每个请求打一行 uvicorn 同格式日志，`0` 关掉 |
| `MUSE2API_IMAGE_TIMEOUT` / `VIDEO_TIMEOUT` / `CHAT_TIMEOUT` | `240` / `600` / `300` | 三类任务的超时 |
| `MUSE2API_PUBLIC_BASE` | 空 | 非空时 `data[].url` 拼成绝对地址，否则回相对路径 `/v1/media/…` |
| `MUSE2API_CORS_ORIGINS` | `https://muse.ai,https://www.muse.ai` | 与上游同 |

**冷启动代价**：默认配置下静置 30 分钟后浏览器会被释放，之后第一条请求要多等 5–15 秒重新起浏览器。不想接受就去 `MUSE2API_WARM_TAB=1`（代价是内存回到上游水平）。

---

## 与上游的差异

代码里主要差异点都带 `DEVIATION:` 注释（配置项与协议层那几条带编号 #1/#19/#20，`grep -rn "DEVIATION:" internal/` 可以列出来），登记在案的共 20 条 —— 下面列的是**会影响你怎么用**的那几条，其余属于措辞、键序、数值宽容度级别。

1. **空闲释放浏览器 + 取消自动预热**（#1）—— 达成低常驻内存的唯一手段，`MUSE2API_BROWSER_IDLE_MINUTES` / `MUSE2API_WARM_TAB` 可关。
2. **不做容器内 git 自更新**（#4）—— `/admin/repo/pull`、`/admin/repo/push`、`/admin/update/upgrade` 只返回版本信息与手动更新步骤，面板对应按钮置灰。上游那两个端点是"在容器里 git pull 然后 `os._exit(0)` 靠重启策略拉起来"，而本项目的部署方式是"编译产物 + docker build"，容器内没有带 `.git` 的工作副本，没有对应物。
3. **`/openapi.json` 与面板是内嵌快照**（#13）—— Go 侧不重造 pydantic 的 JSON Schema 导出。为防漂移，`TestRouteTableMatchesOpenAPIDoc` 会把文档里的 `path × method` 与 Go 路由表严格对账，多一条少一条都红。
4. **`/admin/relogin` 改为排队**（#15）—— 上游那条不持全局浏览器锁，照抄在 Go 里就是数据竞争（实测会读到撕裂的接口值直接崩进程）；空闲时对外结果与上游一致，差别只在浏览器被占用时它等而不拆台。
5. **共享状态加了一层原子快照**（#16）—— 把上游靠 GIL 天然拿到的原子性显式补上（面板轮询、保活协程、HTTP 请求、空闲回收四条线同时碰同一份状态）。
6. **响应头名大小写**（#17）—— Go 的 `net/http` 会把头名规范化成标题式（`Content-Length`），上游发的是全小写。HTTP/1.1 头名大小写不敏感，实测**值**逐字相同。
7. **协议层方法表**（#20）—— 上游实际用的解析器是 httptools/llhttp，只认 35 个方法名，表外（含任何小写写法、`FOO`、RTSP 那批）直接 `400 Invalid HTTP request received.` 且**不进 ASGI、不打 access log**。Go 的 `net/http` 没有这张表，所以本项目显式实现了一份，名单由 53 条实测钉住。
8. **access log**（#19）—— 上游靠 uvicorn 默认输出；Go 版按同一行格式实现，行里的目标串同样是 `quote(unquote(target))`（`%2F`→`/`、`%2e%2e`→`..`，而 `%20`/`%252F` 原样）。

---

## 质量保障：把真 Python 当 oracle

所有对齐断言都是**从跑着的上游实例录制出来的**，不是照着源码读出来的（读过依赖源码去推断框架行为的坑：从 starlette 源码判断 GET 路由接受 HEAD，实测是 405 + `allow: GET`）。

```bash
go test ./...            # 六个包全绿
```

| 金样本 | 内容 |
|---|---|
| `internal/server/testdata/routes_golden.json` | 142 条契约用例（401/404/405/422/500/307/SSE/坏 JSON/Content-Type 变体/媒体删除/字段校验…），其中 90+ 条比对**响应体字节 sha256** |
| `internal/server/testdata/appfunc_golden.json` | 纯函数层（提示词构造、SSE 分帧、`model_dump` 键序、`or` 默认值语义等） |
| `internal/engine/testdata/*.json` | CDP 调用序列：假 page 打进真 `engine.py`，录下它**打算发出的每一条 `Runtime.evaluate` / `Input.dispatch`**，Go 按同序回放；表达式差一个字符、顺序错一位、params 多一个键都会红 |
| `internal/server/testdata/accesslog_golden.json` | 27 行真实 access log（7 行由裸 socket 发出，用来排除客户端改写的解释） |
| `internal/server/testdata/method_table.python.json` | 53 个方法名的实测状态码/正文 |

录制脚本在 `tools/golden/`，需要一台跑着上游 Python 的实例当 oracle（`run_on_nas.mjs` 那类是把脚本送进容器执行、取回 `@@JSON@@` 后的 base64）。**因此 CI 里只能跑回放，跑不了录制** —— 这也正是它的好处：改动代码后回放立刻红，而录制端是只读的、不消耗额度。

每个关键断言都做过**变异验证**：把被测的那一行改坏（例如把视频宽限期 15s 调成 14s、把方法表闸门关掉、把重新转义那步去掉），对应测试必须当场红，否则认为这条测试没咬住、不算数。

---

## 运维提示

- **比内存要用 cgroup 的 `anon`，不要用 `docker stats`。** stats 的口径是 `usage − inactive_file`，会把 Chromium 写盘留下的 page cache 算进去；真实生成之后两边数字甚至会反过来。我们这套部署环境实测：空闲且浏览器释放后 Go 的 `anon` 约 **6–10MB**（上游 Python 同刻 321–414MB），工作态两边都要开浏览器、量级接近。
- **`--init` / `init: true` 不是可选的。** Go 进程当 PID 1 时会收养 Chromium 孤儿子孙，而代码只 `Wait()` 自己那个直接子进程 —— 实测一轮"起浏览器再释放"留下 8 个僵尸进程。
- **看请求**：`docker logs muse2api-go | grep '^INFO:'`（与 uvicorn 逐字节同格式），或 `MUSE2API_ACCESS_LOG=0` 关掉。管理面板里没有请求日志页，上游同样没有。
- **面板的任务列表只含 `async:true` 的生图/编辑和全部视频任务**；同步生图与对话不建任务记录（上游也是）。同步生图想留痕只能看媒体库和日志。

---

## 风险声明

- 本项目**不是 muse.ai 的官方 API**，靠 cookie 驱动其网页端。对方前端结构、DOM 类名、隧道协议、风控随时会变，任何一次改版都可能让它失效；上游也据此频繁发版。
- 用你自己的账号与 cookie，合规性由你自行评估（可能触及服务商的使用条款）。额度、封号等后果本项目不承担任何责任。
- 软件按 **MIT** 提供，无任何保证。派生关系与录制来源见 `NOTICE`。
