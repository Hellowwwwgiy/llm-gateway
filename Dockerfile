# Multi-stage build for both gateway and dispatcher
ARG BINARY=gateway

FROM golang:1.21-alpine AS builder
ARG BINARY
WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/${BINARY} ./cmd/${BINARY}

# Runtime
FROM alpine:3.18
ARG BINARY
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/${BINARY} /app/server
WORKDIR /app

EXPOSE 8080
ENTRYPOINT ["/app/server"]
