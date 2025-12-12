# 多阶段构建 Dockerfile
FROM golang:1.21-alpine AS builder

# 设置工作目录
WORKDIR /app

# 安装必要的包
RUN apk add --no-cache gcc musl-dev sqlite-dev git make

# 复制 go mod 文件
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 构建应用程序
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o openidcs-client ./cmd

# 运行阶段
FROM alpine:latest

# 安装运行时依赖
RUN apk --no-cache add ca-certificates tzdata sqlite

# 设置时区
ENV TZ=Asia/Shanghai

# 创建非root用户
RUN addgroup -g 1001 -S openidcs && \
    adduser -u 1001 -S openidcs -G openidcs

# 创建必要目录
RUN mkdir -p /app/data /app/config /app/logs && \
    chown -R openidcs:openidcs /app

# 复制构建的二进制文件
COPY --from=builder /app/openidcs-client /app/openidcs-client

# 复制配置文件
COPY --from=builder /app/config/config.yaml /app/config/config.yaml

# 切换到非root用户
USER openidcs

# 设置工作目录
WORKDIR /app

# 暴露端口
EXPOSE 1880

# 健康检查
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:1880/health || exit 1

# 启动命令
CMD ["./openidcs-client"]