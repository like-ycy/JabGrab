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

from .models import ScrapeResult  # noqa: E402
from .scraper import BaseScraper  # noqa: E402

__all__ = ["BaseScraper", "ScrapeResult"]
