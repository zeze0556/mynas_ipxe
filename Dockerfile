# syntax=docker/dockerfile:1

# ---- 构建阶段 ----
# 纯 Go 服务（glebarez/sqlite 为 modernc 纯 Go 实现，无需 cgo），可静态编译。
FROM golang:1.23-bookworm AS builder
WORKDIR /src

# 先拉取依赖，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码并编译 linux/amd64 静态二进制
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/ipxe-server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app

COPY --from=builder /out/ipxe-server /app/ipxe-server
# iPXE 二进制（TFTP 下发）与 Web 控制台静态资源，随镜像发布。
# 如需更新 iPXE 二进制，在宿主机执行 build_ipxe.sh 后重新 build 镜像，
# 或在 compose 中挂载 ./assets:/app/assets（此时需保证宿主机 assets/ 含完整 ipxe 二进制）。
COPY assets /app/assets

# 配置文件与 data/ 目录通过 compose 卷挂载，不 bake 进镜像。
# 使用 host 网络时，以下端口直接复用宿主机网络栈：
#   69/udp   TFTP（下发 iPXE NBP）
#   67/udp   DHCP（仅当 config 中 dhcp.enabled=true）
#   8080/tcp HTTP（/boot /c /r /pe 与 Web UI）
ENTRYPOINT ["/app/ipxe-server", "-config", "/app/config.yaml"]
