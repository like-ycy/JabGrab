#!/usr/bin/env bash
#
# 组装发行目录。
#
# 两种形态（默认轻量）：
#
#   scripts/build.sh           轻量包：只有 App，爬取程序与下载器首次使用时按需下载
#   scripts/build.sh --full    整包：额外带 tools/scraper/ 与 tools/dlre/，离线可用
#   scripts/build.sh --legacy-flat
#                              旧扁平布局（三件套平铺在 .app 旁边 / zip 根目录），
#                              只用于排查「旧包用户升级后还能不能用」
#
# 为什么默认轻量：爬取程序 35MB + 下载器 12MB，占整包 91%，而两者都是半年不动的
# 外部产物；App 自己只有 3.5MB。见 docs/release-split-plan.md。
#
# 目录契约（App 侧 go/internal/locate 按同一路径探测）：
#
#   macOS                      Windows
#   JabGrab.app/               jabgrab.exe
#   tools/scraper/{jab-scrape,_internal/}   tools/scraper/{jab-scrape.exe,_internal/}
#   tools/dlre/N_m3u8DL-RE     tools/dlre/N_m3u8DL-RE.exe
#
# 注意 PyInstaller 在 Windows 上产出的是**平铺**结构（jab-scrape.exe 与 _internal
# 同级），所以必须整目录进 tools/scraper/ —— 直接铺到根目录会让两个组件的
# _internal 撞在一起。
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

mode="light"
run_py=1
run_go=1
for arg in "$@"; do
    case "$arg" in
        --full) mode="full" ;;
        --legacy-flat) mode="legacy" ;;
        # CI 已经单独跑过打包与 wails build，这里只需要重新组装目录。
        --no-py) run_py=0 ;;
        --no-go) run_go=0 ;;
        -h | --help)
            sed -n '3,10p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "未知参数：${arg}（可用：--full、--legacy-flat、--no-py、--no-go）" >&2
            exit 2
            ;;
    esac
done

case "$(uname)" in
    Darwin)
        OS="darwin"
        WAILS_PLATFORM="darwin/$(uname -m | sed 's/x86_64/amd64/')"
        DLR_NAME="N_m3u8DL-RE"
        APP_BIN="JabGrab.app/Contents/MacOS/jabgrab"
        ;;
    MINGW* | MSYS* | CYGWIN*)
        OS="windows"
        WAILS_PLATFORM="windows/amd64"
        DLR_NAME="N_m3u8DL-RE.exe"
        APP_BIN="jabgrab.exe"
        ;;
    *)
        echo "只支持 macOS 与 Windows（Git Bash）构建" >&2
        exit 1
        ;;
esac

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

step "1/5 打包 Python 爬取程序（PyInstaller onedir）"
if [ "$run_py" = 1 ]; then
    "$ROOT/scripts/build-py.sh"
else
    [ -x "$PY_BUILD/dist/jab-scrape/jab-scrape" ] || [ -x "$PY_BUILD/dist/jab-scrape/jab-scrape.exe" ] || {
        echo "--no-py 但 python/build/pyi/dist/jab-scrape 不存在，无法组装" >&2
        exit 1
    }
    echo "    跳过（--no-py），复用现有产物"
fi

step "2/5 构建 Go / Wails 应用"
if [ "$run_go" = 1 ]; then
    cd "$GO_DIR"
    # 版本取自 go.mod，避免和依赖里的 wails 版本脱节
    WAILS_VER="$(grep -oE 'wailsapp/wails/v2 v[0-9]+\.[0-9]+\.[0-9]+' go.mod | head -1 | awk '{print $2}')"
    go run "github.com/wailsapp/wails/v2/cmd/wails@${WAILS_VER}" build -clean -platform "$WAILS_PLATFORM" >/dev/null
    echo "    → go/build/bin（${WAILS_PLATFORM}）"
else
    [ -d "$GO_DIR/build/bin" ] || {
        echo "--no-go 但 go/build/bin 不存在，无法组装" >&2
        exit 1
    }
    echo "    跳过（--no-go），复用现有产物"
fi

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
    echo "缺少 go/third_party/${DLR_NAME}（可用 scripts/fetch-dlre.sh 获取对应平台版本）" >&2
    exit 1
}
echo "    → go/third_party/${DLR_NAME}"

step "4/5 组装发行目录（${mode}）"
rm -rf "$DIST"
mkdir -p "$DIST"

if [ "$OS" = "darwin" ]; then
    # .app 保持原样（Wails 已签名）。不往 bundle 内塞 PyInstaller 目录：那会让
    # codesign 把整棵树当嵌套代码校验，签名做不干净，反而导致应用无法启动。
    cp -R "$GO_DIR/build/bin/jabgrab.app" "$DIST/JabGrab.app"
else
    cp "$GO_DIR/build/bin/jabgrab.exe" "$DIST/"
fi

if [ "$mode" = "full" ]; then
    # 组件目录：一份可执行文件 + 它的 _internal，整体作为子目录。
    mkdir -p "$DIST/tools/scraper" "$DIST/tools/dlre"
    cp -R "$PY_BUILD/dist/jab-scrape/." "$DIST/tools/scraper/"
    cp "$GO_DIR/third_party/${DLR_NAME}" "$DIST/tools/dlre/"
    chmod +x "$DIST/tools/scraper/"* "$DIST/tools/dlre/"* 2>/dev/null || true
elif [ "$mode" = "legacy" ]; then
    # 旧扁平布局（v0.3.0 的 zip）：三件套全铺在同一层，用来验证「老用户升级后
    # 仍然可用」。刻意不建 tools/ —— 那会同时存在两份 35MB 的爬取程序。
    if [ "$OS" = "darwin" ]; then
        cp "$GO_DIR/third_party/${DLR_NAME}" "$DIST/${DLR_NAME}"
        cp -R "$PY_BUILD/dist/jab-scrape" "$DIST/python"
        chmod +x "$DIST/${DLR_NAME}" "$DIST/python/jab-scrape"
    else
        cp "$GO_DIR/third_party/${DLR_NAME}" "$DIST/"
        cp -R "$PY_BUILD/dist/jab-scrape/." "$DIST/"
    fi
else
    echo "    轻量包：不含 tools/，首次使用时按需下载爬取程序与下载器" >&2
fi

step "5/5 自检"
if [ "$OS" = "darwin" ]; then
    codesign --verify --verbose=1 "$DIST/JabGrab.app" 2>&1 | tail -2
fi
# 轻量包缺组件是预期结果（退出码 1），所以这里只看输出不看退出码；
# 整包必须三件套齐全，缺了就是打包错了。
"$DIST/$APP_BIN" --selftest || true

printf '\n完成，发行目录：%s（%s）\n' "$DIST" "$(du -sh "$DIST" | cut -f1)"
