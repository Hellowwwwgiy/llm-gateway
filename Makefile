# SmartProxy — 常用命令
# 先确保已安装 Go 1.21+ 和 Docker Desktop

.PHONY: all tidy run-gw run-disp build build-all docker-up docker-down clean

# 下载依赖
tidy:
	go mod tidy

# 本地直接跑 gateway（依赖 Redis/RabbitMQ 已启动）
run-gw:
	go run ./cmd/gateway

run-disp:
	go run ./cmd/dispatcher

# 编译（与 scripts/build.bat 保持一致：trimpath + strip symbols，减体积防误报）
LDFLAGS := -s -w
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gateway ./cmd/gateway
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dispatcher ./cmd/dispatcher

build-all: build

# Docker Compose 一键拉起
docker-up:
	cp .env.example .env 2>/dev/null || true
	docker compose up -d --build

docker-down:
	docker compose down

# 清理
clean:
	rm -rf bin/ *.exe
	docker compose down -v 2>/dev/null || true
