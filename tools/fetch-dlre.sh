#!/usr/bin/env bash
#
# 下载指定平台的 N_m3u8DL-RE 到 third_party/dlre/<os>-<arch>/。
#
# 仓库里已内置 darwin-arm64 与 windows-amd64 两份；只有 macOS Intel 构建需要
# 额外补齐 osx-x64，因此这个脚本只服务于那种场景。
#
# 版本与 sha256 固定在下面，升级下载器时三处一起改：
#   本文件 / third_party/dlre 内的二进制 / tools/build.sh 头部注释
#
# 用法: tools/fetch-dlre.sh <osx-arm64|osx-x64|win-x64> [--dest DIR]
set -euo pipefail

VERSION="v0.6.0-beta"  # 对应 --version 输出 0.6.0+df70f0b3da0c630bd413bf617e758051f6b64757
BUILD="20260629"

usage() {
    echo "用法: $0 <osx-arm64|osx-x64|win-x64> [--dest DIR]" >&2
    exit 2
}

[ $# -ge 1 ] || usage

platform="$1"
shift

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dest=""
while [ $# -gt 0 ]; do
    case "$1" in
        --dest)
            dest="$2"
            shift 2
            ;;
        *) usage ;;
    esac
done

case "$platform" in
    osx-arm64)
        subdir="darwin-arm64"
        ext="tar.gz"
        sha="ca181b7e8976564766c34e3cefe48fff409f58289ca970ade062668c01c53918"
        out="N_m3u8DL-RE"
        ;;
    osx-x64)
        subdir="darwin-amd64"
        ext="tar.gz"
        sha="4dab8d256ae3164557f48a3326c8fa60da99e37536ee1589ce6c841180fa8204"
        out="N_m3u8DL-RE"
        ;;
    win-x64)
        subdir="windows-amd64"
        ext="zip"
        sha="3825fd42ee502f98a9378f6fdddb2f7822709f521806214f466db6935c950f1a"
        out="N_m3u8DL-RE.exe"
        ;;
    *) usage ;;
esac

asset="N_m3u8DL-RE_${VERSION}_${platform}_${BUILD}.${ext}"
url="https://github.com/nilaoda/N_m3u8DL-RE/releases/download/${VERSION}/${asset}"

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '下载 %s\n' "$url"
# GitHub 的 release 资产偶尔 502/连接被重置，重试几次再放弃
curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused -o "$tmp/$asset" "$url"

actual="$(sha256_of "$tmp/$asset")"
if [ "$actual" != "$sha" ]; then
    echo "校验失败：$asset" >&2
    echo "  期望 sha256: $sha" >&2
    echo "  实际 sha256: $actual" >&2
    exit 1
fi
echo "校验通过：$actual"

mkdir -p "$tmp/x"
case "$ext" in
    tar.gz) tar -xzf "$tmp/$asset" -C "$tmp/x" ;;
    zip) unzip -o -q "$tmp/$asset" -d "$tmp/x" ;;
esac

[ -f "$tmp/x/$out" ] || { echo "压缩包里没有 $out" >&2; exit 1; }

# 未显式 --dest 时落到标准位置：third_party/dlre/<os>-<arch>/
[ -n "$dest" ] || dest="$repo/third_party/dlre/$subdir"
mkdir -p "$dest"
install -m 0755 "$tmp/x/$out" "$dest/$out"
printf '已就绪：%s（%s）\n' "$dest/$out" "$(du -h "$dest/$out" | cut -f1)"
