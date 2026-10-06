# muse2api 的 Go 版镜像。
#
# 二进制在开发机上交叉编译好再拷进来，NAS 上不需要装 Go：
#   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
#     go build -trimpath -ldflags="-s -w" -o build/muse2api-go ./cmd/muse2api
# 编译产物约 8.7MB（已 -s -w）。CGO_ENABLED=0 是必须的：静态链接后运行时不需要 glibc 之外的任何东西。
#
# 基础镜像与 Python 版保持同一发行版（bookworm）+ 同一份 chromium 包，
# 两边跑出来的 DOM/渲染行为才可比 —— 我们复刻的是"逻辑一致"，浏览器不能换个版本。
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

COPY build/muse2api-go /usr/local/bin/muse2api-go
# /admin/update/check 与 /admin/status 会读 version.json 报当前版本；没有它退回 "1.5.0"。
COPY deploy/version.json /app/version.json

# 数据目录与 Python 版同布局（data/media、data/downloads，profile 落在 $HOME 下）；
# profile 根目录的取值规矩见 config.go 里 ProfileRoot 那条注释，两边指向同一个位置。
RUN mkdir -p /app/data/media /app/data/downloads /root/muse2api-profiles

VOLUME ["/app/data"]

EXPOSE 18610

CMD ["muse2api-go"]
