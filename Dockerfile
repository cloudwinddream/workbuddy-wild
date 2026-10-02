# WorkBuddy-Wild Web 版 Docker 镜像（多阶段构建）
#
# 构建产物：无头 Web 服务 cmd/webserver
#   - OpenAI 兼容 API（/v1/*，双平台 workbuddy/traework）
#   - 自动签到调度器（双平台）
#   - REST 管理 API（/api/*）+ 浏览器管理页（/）
#
# 说明：
# - 构建阶段用 goproxy.cn（仓库文档实测可用的镜像；直连 proxy.golang.org
#   在部分网络下会 i/o timeout）。
# - CGO_ENABLED=0 产出纯静态二进制，运行镜像只需 alpine。

ARG VERSION=0.6.9-web

FROM golang:1.25-alpine AS builder
ARG VERSION
ENV GOPROXY=https://goproxy.cn,direct \
    GOFLAGS=-mod=mod \
    CGO_ENABLED=0
WORKDIR /src

# 先拷 go.mod/go.sum 做依赖层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 仅构建 webserver（桌面版 main.go 仅限 windows 编译，已用 build tag 隔离）
RUN go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/webserver ./cmd/webserver && \
    /out/webserver -h 2>&1 | head -3

FROM alpine:3.20
# ca-certificates：上游 HTTPS；tzdata：签到按本地时区；wget：健康检查
RUN apk add --no-cache ca-certificates tzdata wget
WORKDIR /app

COPY --from=builder /out/webserver /app/webserver
# 管理页静态文件（桌面版前端原样复用；shim.js 已内嵌进二进制）
COPY frontend/dist /app/frontend/dist

# 默认配置（全部可用环境变量覆盖，见 README-DOCKER.md）
ENV WB2A_LISTEN=":7863" \
    WB2A_AUTH_DIR="/data/auths" \
    WB2A_STATE_FILE="/data/state.json" \
    TZ="Asia/Shanghai"
# WB2A_API_KEY / WB2A_PUBLIC_URL 请在 compose 里按需设置

VOLUME ["/data"]
EXPOSE 7863

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD wget -q -O - http://127.0.0.1:7863/healthz || exit 1

ENTRYPOINT ["/app/webserver"]
