#!/bin/bash
# RelayNet Server 启动脚本
# 用法: ./start.sh [端口号]
# 示例: ./start.sh 19132

PORT=${1:-19132}
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

chmod +x "$SCRIPT_DIR/relayserver"

echo "================================"
echo "  RelayNet Server"
echo "  端口: $PORT"
echo "================================"

exec "$SCRIPT_DIR/relayserver" -listen ":$PORT"
