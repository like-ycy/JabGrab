#!/usr/bin/env bash
#
# 组装发行目录：Go 主程序 + Python 爬取程序 + N_m3u8DL-RE 三件套。
#
# macOS 产出 dist/JabGrab.app（双击即用），三个可执行文件都在
# Contents/MacOS 下并列，运行时按「同级目录 → 配置 → PATH」顺序自动定位。
#
# Windows 建议用 scripts/build.ps1；本脚本在 Git Bash 下也能跑，产物同样落在 dist/。
#
# N_m3u8DL-RE 版本：0.6.0-beta（0.6.0+df70f0b3da0c630bd413bf617e758051f6b64757）
#   go/third_party/N_m3u8DL-RE     → macOS arm64（仓库内置）
#   go/third_party/N_m3u8DL-RE.exe → Windows x64（仓库内置）
#   macOS Intel 需要 osx-x64，由 scripts/fetch-dlre.sh 现拉
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_DIR="$ROOT/go"
DIST="$ROOT/dist"
PY_BUILD="$ROOT/python/build/pyi"

case "$(uname)" in
    Darwin)
        OS="darwin"
        WAILS_PLATFORM="darwin/$(uname -m | sed 's/x86_64/amd64/')"
        DLR_NAME="N_m3u8DL-RE"
        ;;
    MINGW* | MSYS* | CYGWIN*)
        OS="windows"
        WAILS_PLATFORM="windows/amd64"
        DLR_NAME="N_m3u8DL-RE.exe"
        ;;
    *)
        echo "只支持 macOS 与 Windows（Git Bash）构建" >&2
        exit 1
        ;;
esac

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

step "1/5 打包 Python 爬取程序（PyInstaller onedir）"
"$ROOT/scripts/build-py.sh"

step "2/5 构建 Go / Wails 应用"
cd "$GO_DIR"
# 版本取自 go.mod，避免和依赖里的 wails 版本脱节
WAILS_VER="$(grep -oE 'wailsapp/wails/v2 v[0-9]+\.[0-9]+\.[0-9]+' go.mod | head -1 | awk '{print $2}')"
go run "github.com/wailsapp/wails/v2/cmd/wails@${WAILS_VER}" build -clean -platform "$WAILS_PLATFORM" >/dev/null
echo "    → go/build/bin（$WAILS_PLATFORM）"

step "3/5 准备 N_m3u8DL-RE"
# 仓库内置 arm64 版；Intel Mac 上如果发现架构不对就现拉 x64 版。
if [ "$OS" = "darwin" ] && [ "$(uname -m)" = "x86_64" ]; then
    dlr_desc="$(file -b "$GO_DIR/third_party/$DLR_NAME" 2>/dev/null || true)"
    case "$dlr_desc" in
        *x86_64*) ;;
        *)
            echo "    检测到非 x86_64 下载器，拉取 osx-x64"
            "$ROOT/scripts/fetch-dlre.sh" osx-x64 --dest "$GO_DIR/third_party"
            ;;
    esac
fi
[ -f "$GO_DIR/third_party/$DLR_NAME" ] || {
    echo "缺少 go/third_party/$DLR_NAME（可用 scripts/fetch-dlre.sh 获取对应平台版本）" >&2
    exit 1
}
echo "    → go/third_party/$DLR_NAME"

step "4/5 组装发行目录"
rm -rf "$DIST"
mkdir -p "$DIST"

if [ "$OS" = "darwin" ]; then
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

step "5/5 自检"
if [ "$OS" = "darwin" ]; then
    codesign --verify --verbose=1 "$DIST/JabGrab.app" 2>&1 | tail -2
    "$DIST/JabGrab.app/Contents/MacOS/jabgrab" --selftest || true
else
    "$DIST/jabgrab.exe" --selftest || true
fi

printf '\n完成，发行目录：%s（%s）\n' "$DIST" "$(du -sh "$DIST" | cut -f1)"
