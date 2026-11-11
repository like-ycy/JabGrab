#!/usr/bin/env bash
#
# 打包 Python 爬取程序（PyInstaller onedir），macOS / Windows / CI 共用。
# 参数统一从 scripts/pyinstaller.args 读取，产物落在 python/build/pyi/dist/jab-scrape。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY_DIR="$ROOT/python"
PY_BUILD="$PY_DIR/build/pyi"

PY_ARGS=()
while IFS= read -r line; do
    case "$line" in '' | '#'*) continue ;; esac
    PY_ARGS+=("$line")
done <"$ROOT/scripts/pyinstaller.args"

cd "$PY_DIR"
uv sync
rm -rf "$PY_BUILD"
uv run pyinstaller "${PY_ARGS[@]}" >/dev/null
echo "    $(du -sh "$PY_BUILD/dist/jab-scrape" | cut -f1) → python/build/pyi/dist/jab-scrape"
