"""jab-scrape：m3u8 爬取 CLI。

输入页面 URL，通过 seleniumbase (CDP) 抓取并解析，
向 stdout 输出一行 JSON 结果，日志走 stderr。
"""

from .models import ScrapeResult
from .scraper import BaseScraper

__all__ = ["BaseScraper", "ScrapeResult"]
