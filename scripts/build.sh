#!/usr/bin/env bash
#
# 组装发行目录：Go 主程序 + Python 爬取程序 + N_m3u8DL-RE 三件套。
#
# macOS 产出 dist/JabGrab.app（双击即用），三个可执行文件都在
# Contents/MacOS 下并列，运行时按「同级目录 → 配置 → PATH」顺序自动定位。
#
# N_m3u8DL-RE 版本：0.6.0+df70f0b3da0c630bd413bf617e758051f6b64757
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_DIR="$ROOT/go"
PY_DIR="$ROOT/python"
DIST="$ROOT/dist"
PY_BUILD="$PY_DIR/build/pyi"
DLR_NAME="N_m3u8DL-RE"
[ "$(uname)" = "Darwin" ] || DLR_NAME="N_m3u8DL-RE.exe"

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

step "1/4 打包 Python 爬取程序（PyInstaller onedir）"
cd "$PY_DIR"
uv sync
rm -rf "$PY_BUILD"
uv run pyinstaller --noconfirm --clean --onedir --name jab-scrape --paths src \
    --collect-all seleniumbase --collect-all mycdp --collect-all pdbp \
    --exclude-module tkinter --exclude-module _tkinter --exclude-module pydoc \
    --exclude-module pytest --exclude-module _pytest --exclude-module behave \
    --distpath "$PY_BUILD/dist" --workpath "$PY_BUILD/work" --specpath "$PY_BUILD" \
    pyinstaller_entry.py >/dev/null
echo "    $(du -sh "$PY_BUILD/dist/jab-scrape" | cut -f1) → build/pyi/dist/jab-scrape"

step "2/4 构建 Go / Wails 应用"
cd "$GO_DIR"
wails build -s -clean >/dev/null
echo "    → go/build/bin/jabgrab"

step "3/4 组装发行目录"
rm -rf "$DIST"
mkdir -p "$DIST"

if [ "$(uname)" = "Darwin" ]; then
    # .app 保持原样（Wails 已签名），三件套平铺在它旁边。
    # 不往 bundle 内塞 PyInstaller 目录：那会让 codesign 把整棵树当嵌套代码校验，
    # 签名做不干净，反而导致应用无法启动。
    cp -R "$GO_DIR/build/bin/jabgrab.app" "$DIST/JabGrab.app"
    cp "$GO_DIR/third_party/$DLR_NAME" "$DIST/$DLR_NAME"
    rm -rf "$DIST/python"
    cp -R "$PY_BUILD/dist/jab-scrape" "$DIST/python"
    chmod +x "$DIST/$DLR_NAME" "$DIST/python/jab-scrape"
    echo "    → dist/JabGrab.app + $DLR_NAME + python/jab-scrape"
else
    cp "$GO_DIR/build/bin/jabgrab.exe" "$DIST/"
    cp "$GO_DIR/third_party/$DLR_NAME" "$DIST/"
    cp -R "$PY_BUILD/dist/jab-scrape/." "$DIST/"
    echo "    → dist/jabgrab.exe + $DLR_NAME + jab-scrape.exe"
fi

step "4/4 自检"
if [ "$(uname)" = "Darwin" ]; then
    codesign --verify --verbose=1 "$DIST/JabGrab.app" 2>&1 | tail -2
    "$DIST/JabGrab.app/Contents/MacOS/jabgrab" --selftest || true
else
    "$DIST/jabgrab.exe" --selftest || true
fi

printf '\n完成，发行目录：%s（%s）\n' "$DIST" "$(du -sh "$DIST" | cut -f1)"
