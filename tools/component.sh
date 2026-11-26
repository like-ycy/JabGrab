#!/usr/bin/env bash
# 生成组件包（scraper / dlre）并写入 dist/release/。
#
# 由 tools/build.sh 在 PyInstaller 与 wails build 之后调用：
#   tools/component.sh -v 0.8.3
#
# 为什么打包与命名交给 cmd/manifestgen -pack，而不是在这里拼 zip 命令：
# 它会跑一遍产物取自述版本、按自述版本命名、再按 App 侧的解析路径校验包结构。
# 用 shell 拼的话，「包名」与「清单里的版本」就成了两份事实，分叉时的症状是
# 用户装完一直提示可更新，而构建全绿。
set -euo pipefail

# shellcheck source=common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
host_platform

while [[ $# -gt 0 ]]; do
    case "$1" in
        -v | --version)
            # 组件版本取产物自述，不取这个值；接受它是为了与 build.sh 的调用形式一致。
            shift
            ;;
        -h | --help)
            sed -n '3,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) die "未知参数：$1" ;;
    esac
    shift
done

[[ "$HAS_PYTHON" == 1 ]] || die "模板 A 没有组件包"

REL="$ROOT/dist/release"
mkdir -p "$REL"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

# 组件包内顶层是 <name>/，与客户端安装时的剥壳规则（component.TopLevelDir）对应。
mkdir -p "$stage/scraper" "$stage/dlre"
cp -R "$ROOT/python/build/pyi/dist/$PY_BUILD_NAME"/. "$stage/scraper/"

ext=""
[[ "$HOST_OS" == windows ]] && ext=".exe"
dlre_src="$ROOT/third_party/dlre/$HOST_OS-$HOST_ARCH/N_m3u8DL-RE$ext"
[[ -f "$dlre_src" ]] || die "缺少 ${dlre_src}（可用 tools/fetch-dlre.sh 获取对应平台版本）"
cp "$dlre_src" "$stage/dlre/N_m3u8DL-RE$ext"

step "生成组件包（${HOST_OS}/${HOST_ARCH}）"
( cd "$ROOT/go" && go run ./cmd/manifestgen -pack \
    -src "$stage/scraper" -name scraper -binary jab-scrape \
    -protocol 1 -slug "$HOST_OS" -arch "$HOST_ARCH" -out-dir "$REL" )
( cd "$ROOT/go" && go run ./cmd/manifestgen -pack \
    -src "$stage/dlre" -name dlre -binary N_m3u8DL-RE \
    -protocol 1 -slug "$HOST_OS" -arch "$HOST_ARCH" -out-dir "$REL" )

ls -1 "$REL"
