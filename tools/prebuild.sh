#!/usr/bin/env bash
# 预构建钩子：把「本平台的组件清单」烘进 Go 代码，供断网时使用。
#
# 由 tools/build.sh 在组件包就绪之后、wails build 之前调用。App 侧读的是
# go/internal/component/manifest_builtin.json，所以必须在编译前写好。
#
# 只在发布构建里生成：清单里的下载地址形如 releases/download/<tag>，而 Release
# 只对 tag 存在。本地或分支构建生成出来就是一批必然 404 的死链，所以这里宁可
# 不写 —— 仓库里那份空清单会让 App 走在线清单。
#
# CI 通过 RELEASE_TAG / GITHUB_REPOSITORY 注入；本地留空即跳过。
set -euo pipefail

# shellcheck source=common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
host_platform

tag="${RELEASE_TAG:-$(git -C "$ROOT" describe --tags --exact-match 2>/dev/null || true)}"
if [[ -z "$tag" ]]; then
    note "非 tag 构建：保持仓库内的空清单，组件安装走在线清单"
    exit 0
fi

repo="${GITHUB_REPOSITORY:-}"
if [[ -z "$repo" ]]; then
    # 兼容 https://host/o/r(.git)、git@host:o/r、ssh://git@host/o/r 三种写法。
    # 不用 `+?` 这类非贪婪量词：macOS 自带的 BSD sed 不支持，会报
    # "repetition-operator operand invalid"，而这一行只在本地跑发布预演时才执行。
    url="$(git -C "$ROOT" remote get-url origin 2>/dev/null || true)"
    url="${url%.git}"
    url="${url#*://}"
    url="${url#git@}"
    url="${url/:/\/}"
    repo="$(printf '%s' "$url" | awk -F/ '{print $(NF-1) "/" $NF}')"
fi
[[ -n "$repo" ]] || die "无法确定仓库名，请设置 GITHUB_REPOSITORY"

base="https://github.com/${repo}/releases/download/${tag}"
out="$ROOT/go/internal/component/manifest_builtin.json"

note "清单：${base}（仅 ${HOST_OS}/${HOST_ARCH}）"
( cd "$ROOT/go" && go run ./cmd/manifestgen -dir "$ROOT/dist/release" -base-url "$base" \
    -name scraper -binary jab-scrape -merge -only "$HOST_OS/$HOST_ARCH" -out "$out" )
( cd "$ROOT/go" && go run ./cmd/manifestgen -dir "$ROOT/dist/release" -base-url "$base" \
    -name dlre -binary N_m3u8DL-RE -merge -only "$HOST_OS/$HOST_ARCH" -out "$out" )

# 生成失败会留下空文件，编译后就是「没有兜底」，而构建全绿。
size="$(wc -c < "$out" | tr -d ' ')"
[[ "$size" -ge 100 ]] || die "内置清单异常（$size 字节）"
note "已写入 go/internal/component/manifest_builtin.json（$size 字节）"
