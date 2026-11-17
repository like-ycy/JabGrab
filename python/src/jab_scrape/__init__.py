"""jab-scrape：m3u8 爬取 CLI。

输入页面 URL，通过 seleniumbase (CDP) 抓取并解析，
向 stdout 输出一行 JSON 结果，日志走 stderr。
"""

import sys as _sys

# 必须抢在 seleniumbase 之前把 stdio 换成 UTF-8。
#
# Windows 上 Python 按控制台 ANSI 代码页编码 stdout（中文系统 cp936、英文系统
# cp1252），而本程序输出的是 ensure_ascii=False 的 JSON：中文标题会直接
# UnicodeEncodeError，整次爬取失败，且只在标题含中文时才发作。
#
# 只设 PYTHONIOENCODING 不够可靠：seleniumbase 在 import 时就调用 colorama.init()
# 包住当时的 sys.stdout（seleniumbase/__init__.py），之后再改环境变量或 reconfigure
# 都动不了它已经包住的那一层。所以这段必须放在本包所有 import 之前。
for _stream in (_sys.stdout, _sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8")
    except (AttributeError, ValueError):  # 无 stdio（GUI 打包）或流已分离
        pass
del _stream

from ._version import __version__

__all__ = ["BaseScraper", "ScrapeResult", "__version__"]


def __getattr__(name: str):
    """按需导入重导出的名字。

    `jab_scrape.scraper` 一 import 就会拉起 seleniumbase（几秒）。若在这里直接
    `from .scraper import BaseScraper`，那么 `jab-scrape --version` 也要先付这
    几秒 —— 而它存在的意义恰恰是在「装坏了」的现场取版本号，那时 seleniumbase
    可能根本 import 不进来。
    """
    if name == "BaseScraper":
        from .scraper import BaseScraper

        return BaseScraper
    if name == "ScrapeResult":
        from .models import ScrapeResult

        return ScrapeResult
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
