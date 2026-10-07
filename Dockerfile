# muse2api 的 Go 版镜像。
#
# 两阶段构建：编译阶段用官方 golang 镜像把唯一的入口命令静态编出来，运行阶段只留
# 二进制 + chromium。这样 clone 下来 `docker build .` 就能直接用，不需要宿主装 Go
# —— 代价是首次构建要拉一次 golang 基础镜像。
#
# CGO_ENABLED=0 是必须的：静态链接后运行时不依赖 glibc 之外的任何东西。
# 运行期的发行版与 chromium 包刻意与 Python 版保持一致（同为 bookworm + 发行版 chromium）：
# 我们复刻的是"逻辑一致"，浏览器换个版本 DOM 行为就不可比了。
ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src
COPY . .
# go.mod/go.sum 一起进来，正常联网即可解析依赖；如需离线构建，请自行挂 module cache。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/muse2api-go ./cmd/muse2api

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    MUSE2API_HOME=/app \
    MUSE2API_HOST=0.0.0.0 \
    MUSE2API_PORT=18610 \
    MUSE2API_CHROMIUM=/usr/bin/chromium \
    MUSE2API_CDP_PORT=19210

RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    fonts-wqy-zenhei \
    fonts-freefont-ttf \
    procps \
    curl \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/muse2api-go /usr/local/bin/muse2api-go
# /admin/update/check 与 /admin/status 会读 version.json 报当前版本；没有它退回 "1.5.0"。
COPY deploy/version.json /app/version.json

# 数据目录与 Python 版同布局（data/media、data/downloads，profile 落在 $HOME 下）；
# profile 根目录的取值规矩见 config.go 里 ProfileRoot 那条注释，两边指向同一个位置。
RUN mkdir -p /app/data/media /app/data/downloads /root/muse2api-profiles

VOLUME ["/app/data"]

EXPOSE 18610

CMD ["muse2api-go"]
