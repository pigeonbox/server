# PigeonBox server 镜像（纯后端；前端由 ghcr.io/pigeonbox/frontend 分离提供）
#
# ⚠️ 本镜像不含前端静态资源(0.9.0 起"内嵌前端"模式已剔除)：
#   - k8s/compose 前后端分离部署：静态与 API 反代由 frontend 镜像承担
#   - core 对缺失的 ./static 优雅降级(探针/API 全正常,SPA 路径 404),可安全单跑
#
# 依赖经 go.mod 正式版本解析(core/contracts 从 module proxy 拉取,无需本地 replace 链)。
# 构建上下文:pigeonbox 工作区根目录(仅消费 server/ 子目录)。
#   docker build -f server/Dockerfile -t pigeonbox-server .
# GOPROXY 可用 --build-arg GOPROXY=... 覆盖(默认国内加速;CI 海外环境可传空串走默认)。

# Stage 1: Build Go(server;core/contracts 经版本化依赖拉取)
FROM golang:1.26-alpine AS go-builder
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
RUN apk add --no-cache git
COPY server/ ./server/
WORKDIR /src/server
RUN go mod download
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-X 'github.com/pigeonbox/kit/version.Version=${VERSION}' -X 'github.com/pigeonbox/kit/version.BuildCommit=${COMMIT}' -X 'github.com/pigeonbox/kit/version.BuildTime=${BUILD_TIME}' -w -s" \
    -o /out/server ./cmd/server

# Stage 2: Runtime
FROM alpine:3.22
WORKDIR /app
RUN apk --no-cache add ca-certificates tzdata wget && \
    addgroup -g 1000 app && \
    adduser -D -s /bin/sh -u 1000 -G app app && \
    mkdir -p /app/data /app/static /app/config && \
    chown -R app:app /app
COPY --from=go-builder /out/server ./server
# OpenAPI 规范由 core 运行时生成(/openapi.json,openapi_gen.go),无快照文件。
# ./static 保留空目录:core 默认 StaticDir 指向它,缺失时仅 SPA 路径 404(优雅降级)。
COPY server/configs ./config/
# ---- p2pc 设备直传随包（2026-10-14 对称节点 P2a；缺版本时优雅降级不阻塞构建） ----
# CI 传 P2PC_VERSION（p2p 库线 tag，见 server/.p2pc-version；P2b 起归 trainctl 钉版）。
# 按 TARGETARCH 映射 triple 下载并核 sha256（checksums.txt 侧车），失败即构建失败。
# supervisor 候选查找序命中 /app/p2pc（server 二进制同目录）。
ARG TARGETARCH
ARG P2PC_VERSION=""
RUN if [ -n "$P2PC_VERSION" ]; then \
      case "$TARGETARCH" in \
        amd64) TRIPLE=x86_64-unknown-linux-gnu ;; \
        arm64) TRIPLE=aarch64-unknown-linux-gnu ;; \
        *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
      esac && \
      cd /tmp && \
      wget -q -O "p2pc-${TRIPLE}" "https://github.com/pigeonbox/p2p/releases/download/${P2PC_VERSION}/p2pc-${TRIPLE}" && \
      wget -q -O p2pc-checksums.txt "https://github.com/pigeonbox/p2p/releases/download/${P2PC_VERSION}/p2pc-checksums.txt" && \
      grep "p2pc-${TRIPLE}\$" p2pc-checksums.txt | sha256sum -c - && \
      mv "p2pc-${TRIPLE}" /app/p2pc && chmod +x /app/p2pc && chown app:app /app/p2pc && \
      rm -f p2pc-checksums.txt; \
    fi
USER app
EXPOSE 12345
ENV TZ=Asia/Shanghai
# wget --spider 发 HEAD,/live 未注册 HEAD 会 404 导致健康检查永不通过,须显式 GET
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://localhost:12345/live || exit 1
CMD ["./server", "--config", "./config/config.yaml"]
