"""爬取基类：模板方法模式。

基类负责浏览器抓取页面快照（seleniumbase CDP 模式），
子类只需实现 extract() 完成站点特定的解析。
"""

import contextlib
import json
import logging
import re
import sys
from abc import ABC, abstractmethod

from seleniumbase import SB

from .models import PageSnapshot, ScrapeResult

logger = logging.getLogger(__name__)

# 在同一次 JS 求值中采样，避免重定向前后的源码、URL 与请求头混用。
# hls.url 是 Hls.js 当前播放器的清单；resource entries 对应 Network 中的请求。
PAGE_SNAPSHOT_SCRIPT = r"""JSON.stringify({
    html: document.documentElement?.outerHTML || '',
    url: location.href,
    user_agent: navigator.userAgent,
    player_url: typeof window.hls?.url === 'string' ? window.hls.url : '',
    variant_urls: Array.isArray(window.hls?.levels)
        ? window.hls.levels.flatMap(level => Array.isArray(level.url)
            ? level.url : typeof level.url === 'string' ? [level.url] : [])
        : [],
    media_urls: [...new Set(performance.getEntriesByType('resource')
        .map(entry => entry.name).filter(url => /\.m3u8(?:[?#]|$)/i.test(url)))]
})"""

# 页面打开后轮询等待就绪（能解析出 m3u8 与片名）：jable/missav 都有 Cloudflare
# 验证，页面脚本要跑一会儿才把挑战过掉。条件满足就立刻往下走，不再死等固定秒数；
# 上限给足，等太短拿到的还是挑战页（真机上是这条链路最容易失败的地方）。
PAGE_WAIT_MAX_SEC = 15
# 解题后的第二轮等待上限：让标题、演员等 DOM 由页面 JS 渲染完再取源码。
CAPTCHA_WAIT_MAX_SEC = 10
POLL_INTERVAL_SEC = 0.5


class BaseScraper(ABC):
    def scrape(self, url: str) -> ScrapeResult:
        """主流程：抓取 -> 解析 -> 组装下载地址和请求头。"""
        try:
            page = self.fetch_page(url)
        except Exception as e:
            # 异常类名要带出去：Go 侧日志里只有一句「获取失败」时没法排查。
            logger.exception("Selenium 获取 HTML 失败")
            return ScrapeResult(
                ok=False, error=f"获取网页源码失败: {type(e).__name__}: {e}"
            )
        if not page.html:
            return ScrapeResult(ok=False, error="获取网页源码失败")

        try:
            m3u8_url, video_name = self.extract(page)
            headers = self.extra_headers(page)
        except Exception as e:  # 解析容错，错误交给 Go 侧记录
            logger.exception("解析 HTML 出错")
            return ScrapeResult(ok=False, error=f"解析失败: {e}")

        if not m3u8_url or not video_name:
            return ScrapeResult(ok=False, error=f"未能获取有效信息: {url}")

        # 演员名只用于归档目录，取不到不该拖垮整次下载：解析出错时按无演员处理。
        try:
            actors = self.extract_actors(page.html)
        except Exception:
            logger.exception("解析演员名出错，按无演员处理")
            actors = []

        return ScrapeResult(
            ok=True,
            m3u8_url=m3u8_url,
            video_name=video_name,
            headers=headers,
            actors=actors,
        )

    def fetch_page(self, url: str) -> PageSnapshot:
        """seleniumbase CDP 模式获取页面及实际媒体请求。

        SB 会把启动横幅、驱动下载进度等直接 print 到 stdout，
        这里整体重定向到 stderr，保证 stdout 只有最终那一行 JSON。

        page_ready 是就绪判据：第一轮轮询内就绪就跳过
        解题；否则解一次验证码再轮询第二轮。异常向上抛，由 scrape 统一
        转成带类名的错误信息。
        """
        logger.info("正在获取网页源码: %s", url)
        page = None
        with (
            contextlib.redirect_stdout(sys.stderr),
            SB(
                uc=True,
                test=True,
                raise_test_failure=True,
                locale="en",
                headless=True,
            ) as sb,
        ):
            sb.activate_cdp_mode(url)
            page = self._wait_ready(sb, PAGE_WAIT_MAX_SEC)
            if not self.page_ready(page):
                try:
                    sb.solve_captcha()
                except Exception:
                    # 页面本来就没有验证码时，个别版本会抛错；能取到源码就不算失败
                    logger.debug("solve_captcha 跳过", exc_info=True)
                page = self._wait_ready(sb, CAPTCHA_WAIT_MAX_SEC)
        if page is None:
            raise RuntimeError("浏览器未返回页面快照")
        return page

    def _wait_ready(self, sb, budget_sec: float) -> PageSnapshot:
        """按 POLL_INTERVAL_SEC 轮询页面快照，直到就绪或耗尽预算。"""
        page = self.read_page(sb)
        waited = 0.0
        while not self.page_ready(page) and waited < budget_sec:
            sb.sleep(POLL_INTERVAL_SEC)
            waited += POLL_INTERVAL_SEC
            page = self.read_page(sb)
        return page

    @staticmethod
    def read_page(sb) -> PageSnapshot:
        return PageSnapshot(**json.loads(sb.cdp.evaluate(PAGE_SNAPSHOT_SCRIPT)))

    def page_ready(self, page: PageSnapshot) -> bool:
        """钩子方法：能解析出 m3u8 与片名即认为页面就绪（挑战页解析不出来）。"""
        if not page.html:
            return False
        try:
            m3u8, name = self.extract(page)
        except Exception:
            logger.debug("页面尚未解析就绪", exc_info=True)
            return False
        return bool(m3u8 and name)

    @staticmethod
    def extract_group(
        content: str, pattern: str, default: str | None = None
    ) -> str | None:
        """正则提取第一个捕获组。"""
        match = re.search(pattern, content)
        return match.group(1) if match else default

    def extra_headers(self, page: PageSnapshot) -> dict[str, str]:
        """根据来源网页生成下载请求头，经 Go 侧透传给 N_m3u8DL-RE。"""
        return {}

    def extract_actors(self, html: str) -> list[str]:
        """钩子方法：子类可返回页面上的演员名，用于归档到演员子目录。"""
        return []

    @abstractmethod
    def extract(self, page: PageSnapshot) -> tuple[str | None, str | None]:
        """从页面快照提取 (m3u8 地址, 视频名)。"""
