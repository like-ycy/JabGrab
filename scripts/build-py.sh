#!/usr/bin/env bash
#
# 打包 Python 爬取程序（PyInstaller onedir），macOS / Windows / CI 共用。
# 参数统一从 scripts/pyinstaller.args 读取，产物落在 python/build/pyi/dist/jab-scrape。
#
# 打包完会剔除 selenium 自带的三份 Selenium Manager 二进制（四个平台共 16.9MB）。
# 它们一次都不会被调用：SeleniumBase 始终显式把驱动路径传给 ChromeService，
# selenium 那条「没给路径 → 问 Selenium Manager」的分支永远进不去。详见
# docs/release-split-plan.md §10.1。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY_DIR="$ROOT/python"
PY_BUILD="$PY_DIR/build/pyi"
OUT="$PY_BUILD/dist/jab-scrape"

PY_ARGS=()
while IFS= read -r line; do
    case "$line" in '' | '#'*) continue ;; esac
    PY_ARGS+=("$line")
done <"$ROOT/scripts/pyinstaller.args"

cd "$PY_DIR"
uv sync
rm -rf "$PY_BUILD"
uv run pyinstaller "${PY_ARGS[@]}" >/dev/null

before="$(du -sm "$OUT" | cut -f1)"

# 一个 -name 就能覆盖全部六个文件：三份 selenium-manager / .exe，以及
# selenium-manager.cdx.json 与 THIRD-PARTY-NOTICES。
#
# 为什么在打包后删而不是改 PyInstaller 参数：CLI 没有「排除某些 data 文件」的
# 选项，换 .spec 会引入平台分支，而 scripts/pyinstaller.args 的「一份参数三平台
# 共用」是刻意设计。代价只是这 17MB 仍被写盘一次（构建耗时不变）。
find "$OUT" -name 'selenium-manager*' -delete

stray="$(find "$OUT" -name 'selenium-manager*')"
if [ -n "$stray" ]; then
    echo "产物里仍有 selenium-manager：" >&2
    echo "$stray" >&2
    exit 1
fi

# 驱动目录要清空到只剩 __init__.py。
#
# SeleniumBase 在**运行时**把 chromedriver / uc_driver 下载进
# site-packages/seleniumbase/drivers/，而 --collect-all seleniumbase 会把这个
# 目录原样收进产物。于是同一份源码在「跑过爬取的机器」上打出 70MB、在干净的
# CI 上打出 35MB —— 版本号相同、内容不同，且前者把本机的驱动版本塞给了所有人。
# 驱动必须与本机 Chrome 匹配（见 docs/release-split-plan.md §10.3），所以这里
# 恢复到「发布包不带驱动、首次刮削时由 SeleniumBase 自己下」的形态。
DRIVERS="$OUT/_internal/seleniumbase/drivers"
if [ -d "$DRIVERS" ]; then
    find "$DRIVERS" -type f ! -name '__init__.py' ! -name '*.pyc' -delete
    leftover="$(find "$DRIVERS" -type f ! -name '__init__.py' ! -name '*.pyc')"
    if [ -n "$leftover" ]; then
        echo "产物里仍带着驱动文件（版本无法与用户 Chrome 匹配）：" >&2
        echo "$leftover" >&2
        exit 1
    fi
fi

after="$(du -sm "$OUT" | cut -f1)"
echo "    ${before}MB → ${after}MB（剔除 selenium-manager 与运行时驱动）→ python/build/pyi/dist/jab-scrape"
