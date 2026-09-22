# =========================================================================
# SmartProxy Dockerfile  — multi-stage build for gateway + dispatcher
# Build:
#   docker build --build-arg BINARY=gateway -t smartproxy-gateway .
#   docker build --build-arg BINARY=dispatcher -t smartproxy-dispatcher .
# =========================================================================

ARG BINARY=gateway

# ---------- Stage 1: build ----------
FROM golang:1.21-alpine AS builder
ARG BINARY
WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w" \
      -o /out/server ./cmd/${BINARY}

# ---------- Stage 2: runtime ----------
FROM alpine:3.18
ARG BINARY

RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=builder /out/server /app/server
WORKDIR /app

# 两个服务都跑在这两个端口：compose 做映射
EXPOSE 8080 8081

USER 1000
ENTRYPOINT ["/app/server"]
