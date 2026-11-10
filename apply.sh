#!/usr/bin/env bash
# 把本仓库的补丁覆盖到一份官方 frp v0.72.0 源码上。
# 用法: ./apply.sh /path/to/frp-source
set -euo pipefail

SRC="${1:-}"
if [ -z "$SRC" ] || [ ! -d "$SRC" ]; then
  echo "用法: $0 <frp 源码目录>" >&2
  exit 1
fi

DIR="$(cd "$(dirname "$0")" && pwd)"
PATCH="$DIR/frp-patch"

echo ">> 覆盖补丁文件到 $SRC"
cp -r "$PATCH/." "$SRC/"

echo "完成。接下来在该 frp 目录中编译："
echo "  cd web/frps && npm install && npm run build-only && cd ../.."
echo "  # 如需带界面的 frpc：cd web/frpc && npm install && npm run build-only && cd ../.."
echo "  CGO_ENABLED=0 go build -ldflags \"-s -w\" -o dist_bin/frps ./cmd/frps"
echo "  CGO_ENABLED=0 go build -ldflags \"-s -w\" -o dist_bin/frpc ./cmd/frpc"
echo "  # 交叉编译（OpenWrt ARM64）：CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags \"-s -w\" -o dist_bin/frps_linux_arm64 ./cmd/frps"
