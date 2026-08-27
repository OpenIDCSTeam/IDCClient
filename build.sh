#!/bin/bash
# HostAgent-Go 构建脚本 (Linux/macOS)
# 用法: ./build.sh [linux|windows|darwin]

BINARY="hostagent"
MAIN="./cmd/server"

TARGET=${1:-linux}

case $TARGET in
  linux)
    GOOS=linux GOARCH=amd64 OUTPUT="${BINARY}-linux-amd64"
    ;;
  windows)
    GOOS=windows GOARCH=amd64 OUTPUT="${BINARY}-windows-amd64.exe"
    ;;
  darwin)
    GOOS=darwin GOARCH=arm64 OUTPUT="${BINARY}-darwin-arm64"
    ;;
  *)
    echo "用法: $0 [linux|windows|darwin]"
    exit 1
    ;;
esac

echo "正在构建 ${GOOS}/${GOARCH}..."
go mod tidy
GOOS=$GOOS GOARCH=$GOARCH go build -v -o $OUTPUT $MAIN
echo "构建完成: $OUTPUT"
