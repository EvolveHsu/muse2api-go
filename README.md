# muse2api-go

[简体中文](README.zh-CN.md) · English (canonical — the two files are maintained in lockstep)

**A Go port of [muse2api](https://github.com/czg86389-hub/muse2api) (Python / FastAPI)**: it wraps muse.ai's web client into an OpenAI-compatible API — chat, image generation, image editing, video generation — plus the admin panel (account cookie pool, tasks, media library, quota, keepalive).

The goal was **same logic, different language**: routes, status codes, error envelopes, SSE framing, response key order and on-disk formats all follow upstream line by line instead of being redesigned. Every known difference is listed under [Deviations from upstream](#deviations-from-upstream).

---

## Upstream and license

| | |
|---|---|
| Original project | https://github.com/czg86389-hub/muse2api |
| Upstream license | MIT (`LICENSE` is upstream's text verbatim; derivation notes in `NOTICE`) |
| Port baseline | started from upstream `2374061` (1.5.3); synced to upstream `v1.6.3` (`e2b8936`) — includes the 1.6.0 multi-account concurrency rework and the 1.6.2/1.6.3 quota + i18n updates |
| This repository | the Go port; contains no upstream Python source |

The port deliberately avoids "read it, understand it, retype it". The JavaScript injected into the browser is **extracted programmatically** from the deployed `engine.py` via AST by `tools/gen_js.mjs`, which generates `internal/engine/js.go`; the admin panel HTML, `/openapi.json` and `oauth2-redirect.html` are **byte snapshots** taken from the running service (`internal/webui/`). None of that was rewritten, so it cannot be "close enough but subtly different".

---

## Why Chromium is still required

Because this is not an API client — it is **web automation**. Every request ultimately ends up driving the DOM of a muse.ai page through CDP, inside headless Chromium.

We measured the container's memory composition (same machine, same moment): of 523MB resident, the Python application layer was only **57MB**; the remaining ~465MB was the Chromium process tree. So switching languages does not lower memory — what does is **releasing the browser when idle**, which is also this port's number-one behavioural deviation.

We also sniffed the protocol early on (a read-only CDP session attached to the live tab, recording WebSocket frames). Verdict: the browser **cannot be removed**. Generated output streams back into the page over an **encrypted private-protocol tunnel**, whose admission tickets are computed on the fly by JavaScript inside the page; no REST endpoint can substitute for it. Building a browserless client would mean reconstructing the whole key exchange and the in-tunnel protocol from minified frontend code that can change with any release — that road is closed, and there is no reason to probe it twice.

---

## What's implemented

The `openapi.json` snapshot contains **46 operations**: 12 under `/v1/*`, 31 under `/admin/*`, and 3 others (`/`, `/docs`, `/redoc`, …).

```bash
# chat
curl -X POST http://<host>:<port>/v1/chat/completions \
  -H "Authorization: Bearer $MUSE2API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"muse-spark","messages":[{"role":"user","content":"hello"}],"stream":true}'

# image generation (synchronous; "async":true switches to task polling)
curl -X POST http://<host>:<port>/v1/images/generations \
  -H "Authorization: Bearer $MUSE2API_KEY" -H "Content-Type: application/json" \
  -d '{"prompt":"a cat wearing a hat","size":"1024x1024"}'

# video generation (async task, poll with GET /v1/videos/{task_id})
curl -X POST http://<host>:<port>/v1/videos/generations \
  -H "Authorization: Bearer $MUSE2API_KEY" -H "Content-Type: application/json" \
  -d '{"prompt":"a red ball bounces once on a white table","duration":6,"async":true}'
```

> `duration` / `size` / `aspect_ratio` are **folded into the prompt** and handed to muse.ai (e.g. `时长严格为 2 秒` — "duration strictly 2 seconds"); the upstream model decides what actually comes out. Since upstream 1.6.0 a video request is never fulfilled with a cover still: only real video sources are accepted, and if the fetched/downloaded artifact turns out to be an image the task fails with an explicit error (`期望生成视频…拒绝以图片冒充视频交付`) instead of returning a `.jpg`.

---

## Quick start

### 1. Build the image

`Dockerfile` is two-stage: the builder stage compiles the single entrypoint statically with the official `golang` image (`CGO_ENABLED=0`, so runtime depends on nothing beyond glibc), and the runtime stage keeps only the binary plus chromium. **Clone and build — Go is not needed on the host.**

```bash
docker build -t muse2api-go:1.6.3 .
```

The runtime image is `debian:bookworm-slim` plus the distribution's own `chromium` — **same distro and same browser package as upstream**, otherwise DOM behaviour is not comparable; changing the chromium version means changing the thing under test.

### 2. Run the container

```bash
docker run -d --name muse2api-go --restart unless-stopped --init \
  -p 8080:18610 -v $PWD/data-go:/app/data --shm-size=2gb \
  -e MUSE2API_KEY=<your key> muse2api-go:1.6.3
```

None of those three flags is decoration:
- `--init`: as PID 1 the Go process adopts chromium's orphaned descendants, while the code only `Wait()`s on its direct child — one "start browser, then release" cycle left 8 zombies behind in measurement. Let init reap them.
- `--shm-size=2gb`: chromium crashes outright if `/dev/shm` is too small when rendering large images.
- `-v ./data-go:/app/data`: the account store, tasks and generated media all live under that one directory, so replacing the container does not mean re-importing cookies.

To run side by side with the Python version, `deploy/docker-compose.go.yml` is a ready example (separate data dir, host port of your choosing).

To run on bare metal (no Docker), build it yourself:

```bash
go build -trimpath -ldflags="-s -w" -o build/muse2api-go ./cmd/muse2api   # requires Go ≥ 1.27
```

### 3. Import accounts (cookies)

Upstream keeps its cookie pool in `data/accounts.json`, in exactly the same format, so existing accounts can be moved over as-is:

```bash
docker cp <python-container>:/app/data/accounts.json ./data-go/accounts.json
docker restart muse2api-go
```

Then check account status at `http://<host>:8080/admin`, or hit `/admin/accounts/{id}/keepalive` once to verify renewal works.

### 4. Self-check

```bash
curl -s localhost:8080/healthz    # {"status":"ok","time":...}
curl -s localhost:8080/readyz
curl -s -H "Authorization: Bearer $MUSE2API_KEY" localhost:8080/v1/models
```

### 5. CI and prebuilt binaries

- `.github/workflows/ci.yml` runs `gofmt`, `go vet -composites=false ./...`, `go test -count=1 ./...` and `go build -trimpath ./...` on every push and PR. The `-composites=false` is deliberate: that batch of ordered key/value literals in `pyval` omits field names on purpose (key order is what matters), so it is a known cleanup item rather than a misconfigured vet.
- `.github/workflows/release.yml` triggers on `v*` tags and publishes three static binaries — linux/amd64, linux/arm64, windows/amd64 — plus `SHA256SUMS`. Container images are not published; build one with `docker build .`.
- CI can only run the **replay** half of the suite, never the recording half — recording needs a live upstream Python instance as oracle (see [Quality assurance](#quality-assurance-the-real-python-is-the-oracle)).

---

## Configuration

Environment variables keep upstream's names and semantics. Below are the ones this port **adds**, or that you are most likely to touch (full list in `internal/config/config.go`, 22 in total):

| Variable | Default | Notes |
|---|---|---|
| `MUSE2API_KEY` | if empty, generated on first start and written to `data/.env` | inbound auth key |
| `MUSE2API_BROWSER_IDLE_MINUTES` | `30` | **Go-only**: how long the browser may sit idle before being released. `0` disables it |
| `MUSE2API_WARM_TAB` | `0` | **Go-only**: `1` restores upstream's "keep one warm tab resident" behaviour (memory in exchange for cold starts) |
| `MUSE2API_ACCESS_LOG` | `1` | **Go-only**: one log line per request in uvicorn's exact format; `0` turns it off |
| `MUSE2API_IMAGE_TIMEOUT` / `VIDEO_TIMEOUT` / `CHAT_TIMEOUT` | `240` / `600` / `300` | timeouts for the three task types |
| `MUSE2API_CONCURRENCY` | `2` | **upstream 1.6.0**: how many generation tasks may run in parallel. Each task gets its own Incognito BrowserContext; the same account is never used by two tasks at once. The code default matches upstream; `deploy/docker-compose.go.yml` pins it to `1`, because each extra lane is another renderer and the browser working set dominates memory |
| `MUSE2API_PUBLIC_BASE` | empty | when set, `data[].url` becomes absolute; otherwise it returns the relative `/v1/media/…` |
| `MUSE2API_CORS_ORIGINS` | `https://muse.ai,https://www.muse.ai` | same as upstream |

**Cold-start cost**: with defaults the browser is released after 30 idle minutes, and the next first request waits an extra 5–15 seconds while it restarts. If you would rather not pay that, set `MUSE2API_WARM_TAB=1` — at the cost of memory returning to upstream levels.

---

## Deviations from upstream

Differences are annotated in the code with `DEVIATION` comments — `grep -rn DEVIATION internal/ cmd/` returns 25 lines: 19 are the annotations themselves and 6 are pointers to them. Nine of the 19 carry numbers (#1, #4, #13–#17, #19, #20); the numbering comes from the ledger kept while porting. The rest of that ledger is wording, key order and numeric-tolerance level: they do not change how the service is used, so they were left as unnumbered comments.

1. **Idle browser release + no automatic warm-up** (#1) — the only thing that actually delivers low resident memory; controlled by `MUSE2API_BROWSER_IDLE_MINUTES` / `MUSE2API_WARM_TAB`.
2. **No in-container git self-update** (#4) — `/admin/repo/pull`, `/admin/repo/push` and `/admin/update/upgrade` return version info and manual update steps only, and the matching panel buttons are greyed out. Upstream's endpoints do "git pull inside the container, then `os._exit(0)` and let the restart policy bring it back"; this port deploys as compiled artifact + docker build, so there is no working copy with a `.git` inside the container and nothing to map onto.
3. **`/openapi.json` and the panel are vendored snapshots** (#13) — the Go side does not reimplement pydantic's JSON Schema export. To catch drift, `TestRouteTableMatchesOpenAPIDoc` reconciles `path × method` from the document against the Go route table; one extra or one missing entry fails. The sibling `/redoc` and `/docs/oauth2-redirect` bodies are not yet pinned by a recording — tracked as #14.
4. **`/admin/relogin` now queues** (#15) — upstream's version does not hold the global browser lock, and copying that into Go is a data race (measured: a torn interface value read crashes the process). It now waits instead of undermining the running task; idle-time external behaviour is identical to upstream.
5. **One extra layer of atomic snapshot for shared state** (#16) — makes explicit the atomicity upstream gets for free from the GIL (panel polling, keepalive goroutine, HTTP requests and the idle reaper all touch the same state concurrently).
6. **Response header name casing** (#17) — Go's `net/http` canonicalizes header names to title case (`Content-Length`), while upstream sends all-lowercase. HTTP/1.1 header names are case-insensitive and the measured **values** are byte-identical.
7. **Protocol-level method table** (#20) — upstream's actual parser is httptools/llhttp, which accepts only 35 method names. Anything outside that table (including any lowercase spelling, `FOO`, the RTSP set) gets `400 Invalid HTTP request received.` **before ASGI, with no access log line**. Go's `net/http` has no such table, so this project implements one explicitly, pinned by 53 measured cases.
8. **Access log** (#19) — upstream relies on uvicorn's default output; the Go port emits the same line format, including the `quote(unquote(target))` treatment of the target (`%2F`→`/`, `%2e%2e`→`..`, while `%20` / `%252F` stay as-is).

---

## Quality assurance: the real Python is the oracle

Every alignment assertion is **recorded from a running upstream instance**, not read out of source code. (Inferring framework behaviour from dependency source has burned us: from starlette's source a GET route should accept HEAD, but the live service answers `405` + `allow: GET`.)

```bash
go test ./...            # all six packages green
```

| Golden fixture | Contents |
|---|---|
| `internal/server/testdata/routes_golden.json` | 142 contract cases (401/404/405/422/500/307/SSE/malformed JSON/Content-Type variants/media deletion/field validation…), 90+ of which compare the **response body's byte sha256** |
| `internal/server/testdata/appfunc_golden.json` | pure-function layer (prompt construction, SSE framing, `model_dump` key order, `or` default-value semantics, etc.) |
| `internal/engine/testdata/*.json` | CDP call sequences: a fake page is injected into the real `engine.py`, recording **every `Runtime.evaluate` / `Input.dispatch` it intends to send**; Go replays the same sequence — one character off in an expression, one swap in order, one extra key in params, and it fails |
| `internal/server/testdata/accesslog_golden.json` | 27 real access log lines (7 sent over a raw socket, to rule out "the client rewrote it" as an explanation) |
| `internal/server/testdata/method_table.python.json` | measured status codes and bodies for 53 method names |

Recording scripts live in `tools/golden/` and need an instance of upstream Python running as oracle (the `run_on_nas.mjs` pattern ships a script into the container, executes it there, and takes back the base64 that follows `@@JSON@@`). **That is also why CI runs replays only, never recordings** — which is the point: break the code and replay fails immediately, while the recording side is read-only and never spends generation quota.

Every key assertion has been **mutation-verified**: break the line under test on purpose (turn the video grace period from 15s into 14s, disable the method-table gate, drop the re-escaping step) and the corresponding test must fail on the spot. Otherwise the test is not holding anything and does not count.

---

## Operational notes

- **Compare memory with cgroup `anon`, not `docker stats`.** stats reports `usage − inactive_file`, which counts the page cache chromium left behind on disk writes; after real generation the two numbers can even invert. Measured on this deployment: idle with the browser released, Go's `anon` is about **6–10MB** (upstream Python at the same moment: 321–414MB). Under load both open a browser and land in the same order of magnitude.
- **`--init` / `init: true` is not optional.** As PID 1 the Go process adopts chromium's orphaned descendants while the code only `Wait()`s on its direct child — one "start then release" cycle left 8 zombies behind.
- **Reading requests**: `docker logs muse2api-go | grep '^INFO:'` (byte-for-byte uvicorn's format), or set `MUSE2API_ACCESS_LOG=0`. There is no request-log page in the admin panel — upstream does not have one either.
- **The panel's task list covers only `async:true` image generation/editing and all video tasks**; synchronous image generation and chat create no task records (upstream likewise). To keep score of a synchronous generation you only have the media library and the logs.

---

## Risk statement

- This project is **not an official muse.ai API**. It drives the web client with cookies. Upstream's frontend structure, DOM class names, tunnel protocol and risk controls can change at any time, and any such change can break it — upstream itself ships frequently for exactly that reason.
- Use your own accounts and cookies, and assess compliance yourself (this may well touch the provider's terms of use). Quota consumption, account bans and other consequences are not this project's responsibility.
- The software is provided under **MIT**, without any warranty. See `NOTICE` for the derivation and how the fixtures were recorded.
