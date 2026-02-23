#!/bin/bash

# LANShare Wails 构建脚本 - 同时构建 64 位和 32 位版本

set -e

echo "==========================================="
echo "     LANShare 构建脚本 (64-bit + 32-bit)"
echo "==========================================="

# 检查 wails 是否安装
if ! command -v wails &> /dev/null; then
    echo "错误: 未找到 wails CLI，请先安装: go install github.com/wailsapp/wails/v2/cmd/wails@latest"
    exit 1
fi

BUILD_DIR="build/bin"
mkdir -p "$BUILD_DIR"

# ---- 64-bit 构建 ----
echo ""
echo ">>> 构建 64-bit 版本..."
wails build -platform windows/amd64

if [ -f "$BUILD_DIR/LANShare.exe" ]; then
    SIZE=$(stat -c%s "$BUILD_DIR/LANShare.exe" 2>/dev/null || stat -f%z "$BUILD_DIR/LANShare.exe" 2>/dev/null || echo 0)
    SIZE_MB=$(awk "BEGIN{printf \"%.1f\", $SIZE/1024/1024}")
    echo "  ✓ LANShare.exe (64-bit) - ${SIZE_MB}MB"
else
    echo "  ✗ 64-bit 构建失败"
    exit 1
fi

# 暂存 64-bit 版本
cp "$BUILD_DIR/LANShare.exe" "$BUILD_DIR/LANShare_64bit.exe.tmp"

# ---- 32-bit 构建 ----
echo ""
echo ">>> 构建 32-bit 版本..."
wails build -platform windows/386

if [ -f "$BUILD_DIR/LANShare.exe" ]; then
    mv "$BUILD_DIR/LANShare.exe" "$BUILD_DIR/LANShare_32bit.exe"
    SIZE=$(stat -c%s "$BUILD_DIR/LANShare_32bit.exe" 2>/dev/null || stat -f%z "$BUILD_DIR/LANShare_32bit.exe" 2>/dev/null || echo 0)
    SIZE_MB=$(awk "BEGIN{printf \"%.1f\", $SIZE/1024/1024}")
    echo "  ✓ LANShare_32bit.exe (32-bit) - ${SIZE_MB}MB"
else
    echo "  ✗ 32-bit 构建失败"
    # 恢复 64-bit 版本
    mv "$BUILD_DIR/LANShare_64bit.exe.tmp" "$BUILD_DIR/LANShare.exe"
    exit 1
fi

# 恢复 64-bit 为默认版本
mv "$BUILD_DIR/LANShare_64bit.exe.tmp" "$BUILD_DIR/LANShare.exe"

echo ""
echo "==========================================="
echo "           构建完成"
echo "==========================================="
echo "  $BUILD_DIR/LANShare.exe        (64-bit)"
echo "  $BUILD_DIR/LANShare_32bit.exe  (32-bit)"
echo "==========================================="
