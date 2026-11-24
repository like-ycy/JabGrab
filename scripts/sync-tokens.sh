#!/usr/bin/env bash
#
# 把 xwang-ui 的 tokens.css 同步到前端目录。
#
# JabGrab 没有构建步骤（wails.json 里 frontend:build 是空串），所以 token 只能以
# 「生成物入库」的方式派生：本脚本按 tag 从 xwang-ui 取源文件，原样写入
# go/frontend/dist/tokens.css，供 index.html 在 style.css 之前直接 <link>。
#
# 用法：
#   scripts/sync-tokens.sh          重新生成 dist/tokens.css
#   scripts/sync-tokens.sh --check  只比对不写；有漂移则非零退出（可用于本地自查）
#
# 升级 xwang-ui：只改下面这一行 TAG，然后重新跑一次本脚本。

set -euo pipefail

XWANG_UI_TAG="v0.6.0"

# xwang-ui 的工作副本默认取与本仓库同级的那一份，可用环境变量覆盖。
XWANG_UI_REPO="${XWANG_UI_REPO:-}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="$ROOT/go/frontend/dist/tokens.css"
SOURCE_PATH="src/tokens.css"

die() {
    echo "sync-tokens: $*" >&2
    exit 1
}

if [ -z "$XWANG_UI_REPO" ]; then
    XWANG_UI_REPO="$(cd "$ROOT/../xwang-ui" 2>/dev/null && pwd || true)"
fi

[ -n "$XWANG_UI_REPO" ] && [ -d "$XWANG_UI_REPO/.git" ] \
    || die "找不到 xwang-ui 仓库，请设置 XWANG_UI_REPO=/path/to/xwang-ui"

commit="$(git -C "$XWANG_UI_REPO" rev-parse --short "$XWANG_UI_TAG" 2>/dev/null)" \
    || die "xwang-ui 里没有 tag ${XWANG_UI_TAG}（先 git fetch --tags）"

# 按 tag 取，不读工作区：工作区的 src/tokens.css 可能正改到一半。
body="$(git -C "$XWANG_UI_REPO" show "$XWANG_UI_TAG:$SOURCE_PATH")" \
    || die "读取 $XWANG_UI_TAG:$SOURCE_PATH 失败"

# 头部注释随每次生成变化（含时间），--check 只比对头部之后的内容，
# 否则「生成时间」本身就会被当成漂移。
header="/* xwang-ui design tokens —— 生成物，请勿手改。
 *
 * 来源：xwang-ui@$XWANG_UI_TAG ($commit) $SOURCE_PATH
 * 生成：$(date '+%Y-%m-%d %H:%M:%S %z')
 * 重新生成：scripts/sync-tokens.sh
 * 校验漂移：scripts/sync-tokens.sh --check
 *
 * 下面是源文件原文，一个字符都不要动；要改取值请去 xwang-ui。
 */"

render() {
    printf '%s\n%s\n' "$header" "$body"
}

# 去掉首段注释后的正文；render 与 --check 都走这里，保证两边口径一致。
strip_header() {
    awk 'f { print } /^ \*\/$/ { f = 1 }'
}

if [ "${1:-}" = "--check" ]; then
    [ -f "$TARGET" ] || die "$TARGET 不存在，先跑一次不带参数的 scripts/sync-tokens.sh"
    if diff -u <(strip_header <"$TARGET") <(printf '%s\n' "$body"); then
        echo "sync-tokens: 无漂移（$XWANG_UI_TAG / ${commit}）"
        exit 0
    fi
    echo "sync-tokens: dist/tokens.css 与 $XWANG_UI_TAG 有漂移，跑一次不带参数的脚本即可修复" >&2
    exit 1
fi

[ "${1:-}" = "" ] || die "未知参数：${1}（只支持 --check）"

render >"$TARGET"
echo "sync-tokens: 已写入 ${TARGET}（$XWANG_UI_TAG / ${commit}）"
