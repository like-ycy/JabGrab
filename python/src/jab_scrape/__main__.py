"""CLI 入口：jab-scrape <url> --source <jable|missav>"""

import argparse
import importlib
import json
import logging
import sys
from typing import TYPE_CHECKING

from ._version import __version__

if TYPE_CHECKING:
    from .scraper import BaseScraper

# 站点名 → 实现类所在的 (模块, 类名)。
#
# 只记名字、不 import：jable/missav 都会拉起 seleniumbase（几秒）。--version 与
# --help 不该为此等待，也不该在 seleniumbase 装坏时跟着一起失败。
SOURCE_MODULES: dict[str, tuple[str, str]] = {
    "jable": ("jable", "JableScraper"),
    "missav": ("missav", "MissAVScraper"),
}

# 已解析的实现类。测试通过向它注入假实现来绕过真实浏览器。
SCRAPERS: dict[str, type["BaseScraper"]] = {}

# 第三方库在 DEBUG 级别会刷屏（websockets 的每条 CLOSE 帧都打一行），
# 即使 -v 也压到 WARNING，只保留本项目的调试日志。
NOISY_LOGGERS = ("websockets", "urllib3", "selenium", "seleniumbase", "asyncio")


def _scraper_class(source: str) -> type["BaseScraper"]:
    """取站点实现类，首次使用时才 import 对应模块。"""
    cached = SCRAPERS.get(source)
    if cached is not None:
        return cached
    module_name, class_name = SOURCE_MODULES[source]
    module = importlib.import_module(f".{module_name}", __package__)
    cls = getattr(module, class_name)
    SCRAPERS[source] = cls
    return cls


def _selftest() -> int:
    """校验冻结产物里的模块链是否齐全，但不启动浏览器。

    为什么需要它：`--help` 只走 argparse，`--version` 连 seleniumbase 都不碰，
    两者都通不过「产物里少了某个模块」的检查。真实翻车案例是把 jable/missav
    改成惰性导入后忘了 `--hidden-import`：PyInstaller 静态分析看不到它们，
    产物里没有这两个子模块，而 `--help`/`--version` 一切正常，只有用户点
    「开始」时才 ModuleNotFoundError。这一步把它们逐个 import 一遍。
    """
    import platform

    print(f"version: {__version__}")
    print(f"python: {platform.python_version()}")

    failures = 0
    for source in sorted(SOURCE_MODULES):
        try:
            cls = _scraper_class(source)
        except Exception as exc:  # noqa: BLE001 — 自检要把任何异常都报出来
            failures += 1
            print(f"{source}: 失败 — {type(exc).__name__}: {exc}")
            continue
        print(f"{source}: ok（{cls.__module__}.{cls.__name__}）")

    try:
        import seleniumbase

        print(f"seleniumbase: {getattr(seleniumbase, '__version__', 'unknown')}")
    except Exception as exc:  # noqa: BLE001
        failures += 1
        print(f"seleniumbase: 失败 — {type(exc).__name__}: {exc}")

    return 0 if failures == 0 else 1


def main(argv: list[str] | None = None) -> int:
    # --version 与 --selftest 在解析参数之前短路：它们要能在任何环境下给出答案，
    # 包括 seleniumbase 资源缺失、甚至只解压了一半的产物。
    # --version 只输出一行版本号，CI 直接 `| head -1` 拿它当组件自述。
    raw = sys.argv[1:] if argv is None else list(argv)
    if "--version" in raw or "-V" in raw:
        print(__version__)
        return 0
    if "--selftest" in raw:
        return _selftest()

    parser = argparse.ArgumentParser(
        prog="jab-scrape",
        description="爬取页面，输出 m3u8 地址与标题 (JSON to stdout)",
    )
    parser.add_argument("url", nargs="?", help="视频页面 URL")
    parser.add_argument(
        "--source",
        choices=sorted(SOURCE_MODULES),
        help="站点类型，决定使用哪个解析器",
    )
    parser.add_argument("-v", "--verbose", action="store_true", help="调试日志")
    parser.add_argument(
        "-V",
        "--version",
        action="version",
        version=__version__,
        help="打印版本号后退出",
    )
    args = parser.parse_args(raw)

    if not args.url or not args.source:
        parser.error("必须给出 url 与 --source")

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        stream=sys.stderr,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    for name in NOISY_LOGGERS:
        logging.getLogger(name).setLevel(logging.WARNING)

    result = _scraper_class(args.source)().scrape(args.url)
    print(json.dumps(result.to_dict(), ensure_ascii=False))
    return 0 if result.ok else 1


if __name__ == "__main__":
    sys.exit(main())
