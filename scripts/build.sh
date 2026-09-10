#!/usr/bin/env bash
# =========================================================================
# SmartProxy — Linux / macOS 一键构建脚本
# 产物放在 dist/ 目录
# =========================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/.."

TARGET="${1:-all}"
OUTPUT_DIR="dist"
VERSION="$(date +%Y%m%d)"

if ! command -v go &> /dev/null; then
    echo "[ERROR] Go not found. Install Go 1.21+"
    exit 1
fi

echo "[INFO] go=$(go version)"
echo "[INFO] target=$TARGET  version=$VERSION"

mkdir -p "$OUTPUT_DIR"
go clean -cache -testcache >/dev/null 2>&1

LDFLAGS="-s -w -X main.Version=$VERSION"

build_one() {
    local name="$1"
    local out="$2"
    echo "[BUILD] $name ..."
    if go build -trimpath -ldflags "$LDFLAGS" -o "$out" "./cmd/$name"; then
        echo "  [OK]  $(ls -lh "$out" | awk '{print $5, $9}')"
    else
        echo "  [FAIL] $name"
        exit 1
    fi
}

case "$TARGET" in
    all)
        build_one gateway    "$OUTPUT_DIR/smartproxy-gateway"
        build_one dispatcher "$OUTPUT_DIR/smartproxy-dispatcher"
        ;;
    gateway)   build_one gateway    "$OUTPUT_DIR/smartproxy-gateway" ;;
    disp|dispatcher) build_one dispatcher "$OUTPUT_DIR/smartproxy-dispatcher" ;;
    *)
        echo "Usage: $0 [all|gateway|dispatcher]"
        exit 1
        ;;
esac

echo ""
echo "[OK] done. artifacts:"
ls -lh "$OUTPUT_DIR"
