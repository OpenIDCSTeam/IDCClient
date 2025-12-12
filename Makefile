# OpenIDCS 客户端 Makefile

.PHONY: build run test clean docker-build docker-run deps fmt vet lint

# 变量定义
APP_NAME := openidcs-client
VERSION := $(shell git describe --tags --always --dirty)
BUILD_TIME := $(shell date +%Y-%m-%d_%H:%M:%S)
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

# 默认目标
all: build

# 安装依赖
deps:
	go mod download
	go mod tidy

# 代码格式化
fmt:
	go fmt ./...

# 代码检查
vet:
	go vet ./...

# 静态分析
lint:
	golangci-lint run

# 运行测试
test:
	go test -v ./...

# 运行测试并生成覆盖率报告
test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# 构建
build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd

# 构建发布版本
build-release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(APP_NAME)-linux-amd64 ./cmd
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o bin/$(APP_NAME)-windows-amd64.exe ./cmd
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o bin/$(APP_NAME)-darwin-amd64 ./cmd

# 运行
run:
	go run ./cmd

# 运行开发模式
dev:
	OPENIDCS_LOG_LEVEL=debug go run ./cmd

# 清理
clean:
	rm -rf bin/
	rm -f coverage.out coverage.html
	rm -f openidcs-client

# Docker构建
docker-build:
	docker build -t $(APP_NAME):$(VERSION) .
	docker tag $(APP_NAME):$(VERSION) $(APP_NAME):latest

# Docker运行
docker-run:
	docker run -d \
		--name $(APP_NAME) \
		-p 1880:1880 \
		-v $(PWD)/data:/app/data \
		-v $(PWD)/config:/app/config \
		$(APP_NAME):latest

# Docker Compose
docker-compose-up:
	docker-compose up -d

docker-compose-down:
	docker-compose down

docker-compose-logs:
	docker-compose logs -f

# 安装开发工具
install-tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install github.com/golang/mock/mockgen@latest

# 生成Mock文件
generate:
	go generate ./...

# 安装到系统
install: build
	sudo cp bin/$(APP_NAME) /usr/local/bin/
	sudo chmod +x /usr/local/bin/$(APP_NAME)

# 卸载
uninstall:
	sudo rm -f /usr/local/bin/$(APP_NAME)

# 打包发布
package:
	mkdir -p release
	tar -czf release/$(APP_NAME)-$(VERSION)-linux-amd64.tar.gz -C bin $(APP_NAME)-linux-amd64
	tar -czf release/$(APP_NAME)-$(VERSION)-darwin-amd64.tar.gz -C bin $(APP_NAME)-darwin-amd64
	zip -j release/$(APP_NAME)-$(VERSION)-windows-amd64.zip bin/$(APP_NAME)-windows-amd64.exe

# 检查依赖更新
deps-update:
	go list -u -m all

# 安全检查
security:
	gosec ./...

# 性能分析
profile:
	go tool pprof http://localhost:1880/debug/pprof/profile

# 帮助
help:
	@echo "可用的命令:"
	@echo "  build          - 构建应用程序"
	@echo "  run            - 运行应用程序"
	@echo "  dev            - 开发模式运行"
	@echo "  test           - 运行测试"
	@echo "  test-coverage  - 运行测试并生成覆盖率报告"
	@echo "  clean          - 清理构建文件"
	@echo "  docker-build   - 构建Docker镜像"
	@echo "  docker-run     - 运行Docker容器"
	@echo "  fmt            - 格式化代码"
	@echo "  vet            - 代码检查"
	@echo "  lint           - 静态分析"
	@echo "  package        - 打包发布"
	@echo "  help           - 显示此帮助信息"