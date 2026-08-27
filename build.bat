@echo off
REM HostAgent-Go 构建脚本 (Windows)
REM 用法: build.bat [linux|windows|darwin]

set BINARY=hostagent
set MODULE=github.com/OpenIDCSTeam/HostAgent-Go
set MAIN=./cmd/server

if "%1"=="linux" (
    set GOOS=linux
    set GOARCH=amd64
    set OUTPUT=%BINARY%-linux-amd64
) else if "%1"=="darwin" (
    set GOOS=darwin
    set GOARCH=arm64
    set OUTPUT=%BINARY%-darwin-arm64
) else (
    set GOOS=windows
    set GOARCH=amd64
    set OUTPUT=%BINARY%-windows-amd64.exe
)

echo 正在构建 %GOOS%/%GOARCH%...
go mod tidy
go build -v -o %OUTPUT% %MAIN%
echo 构建完成: %OUTPUT%
