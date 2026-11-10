"""CLI 入口：jab-scrape <url> --source <jable|missav>"""

import argparse
import json
import logging
import sys

from .jable import JableScraper
from .missav import MissAVScraper
from .scraper import BaseScraper

SCRAPERS: dict[str, type[BaseScraper]] = {
    "jable": JableScraper,
    "missav": MissAVScraper,
}

# 第三方库在 DEBUG 级别会刷屏（websockets 的每条 CLOSE 帧都打一行），
# 即使 -v 也压到 WARNING，只保留本项目的调试日志。
NOISY_LOGGERS = ("websockets", "urllib3", "selenium", "seleniumbase", "asyncio")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="jab-scrape",
        description="爬取页面，输出 m3u8 地址与标题 (JSON to stdout)",
    )
    parser.add_argument("url", help="视频页面 URL")
    parser.add_argument(
        "--source",
        required=True,
        choices=sorted(SCRAPERS),
        help="站点类型，决定使用哪个解析器",
    )
    parser.add_argument("-v", "--verbose", action="store_true", help="调试日志")
    args = parser.parse_args(argv)

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        stream=sys.stderr,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    for name in NOISY_LOGGERS:
        logging.getLogger(name).setLevel(logging.WARNING)

    result = SCRAPERS[args.source]().scrape(args.url)
    print(json.dumps(result.to_dict(), ensure_ascii=False))
    return 0 if result.ok else 1


if __name__ == "__main__":
    sys.exit(main())
