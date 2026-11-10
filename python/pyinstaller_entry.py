"""PyInstaller 打包入口：行为与 `jab-scrape` 命令行一致。

打包命令见项目根目录 scripts/build.sh。
"""

import sys

from jab_scrape.__main__ import main

if __name__ == "__main__":
    sys.exit(main())
