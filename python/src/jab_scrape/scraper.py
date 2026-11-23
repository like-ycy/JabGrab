"""爬取基类：模板方法模式。

基类负责浏览器抓取 HTML（seleniumbase CDP 模式），
子类只需实现 extract() 完成站点特定的解析。
"""

import contextlib
import logging
import re
import sys
from abc import ABC, abstractmethod
from urllib.request import Request, urlopen

from seleniumbase import SB

from .models import ScrapeResult

logger = logging.getLogger(__name__)

UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
)

# 页面打开后轮询等待就绪（能解析出 m3u8 与片名）：jable/missav 都有 Cloudflare
# 验证，页面脚本要跑一会儿才把挑战过掉。条件满足就立刻往下走，不再死等固定秒数；
# 上限给足，等太短拿到的还是挑战页（真机上是这条链路最容易失败的地方）。
PAGE_WAIT_MAX_SEC = 15
# 解题后的第二轮等待上限：让标题、演员等 DOM 由页面 JS 渲染完再取源码。
CAPTCHA_WAIT_MAX_SEC = 10
POLL_INTERVAL_SEC = 0.5


class BaseScraper(ABC):
    def scrape(self, url: str) -> ScrapeResult:
        """主流程：抓取 -> 解析 -> 修正 -> 组装结果。"""
        try:
            html = self.fetch_html(url, ready=self.page_ready)
        except Exception as e:
            # 异常类名要带出去：Go 侧日志里只有一句「获取失败」时没法排查。
            logger.exception("Selenium 获取 HTML 失败")
            return ScrapeResult(
                ok=False, error=f"获取网页源码失败: {type(e).__name__}: {e}"
            )
        if not html:
            return ScrapeResult(ok=False, error="获取网页源码失败")

        try:
            m3u8_url, video_name = self.extract(html)
        except Exception as e:  # 解析容错，错误交给 Go 侧记录
            logger.exception("解析 HTML 出错")
            return ScrapeResult(ok=False, error=f"解析失败: {e}")

        if not m3u8_url or not video_name:
            return ScrapeResult(ok=False, error=f"未能获取有效信息: {url}")

        # 演员名只用于归档目录，取不到不该拖垮整次下载：解析出错时按无演员处理。
        try:
            actors = self.extract_actors(html)
        except Exception:
            logger.exception("解析演员名出错，按无演员处理")
            actors = []

        m3u8_url = self.fix_m3u8(m3u8_url)
        return ScrapeResult(
            ok=True,
            m3u8_url=m3u8_url,
            video_name=video_name,
            headers=self.extra_headers(m3u8_url),
            actors=actors,
        )

    @staticmethod
    def fetch_html(url: str, ready=None) -> str:
        """seleniumbase CDP 模式获取页面源码。

        SB 会把启动横幅、驱动下载进度等直接 print 到 stdout，
        这里整体重定向到 stderr，保证 stdout 只有最终那一行 JSON。

        ready 是就绪判据（scrape 传入 page_ready）：第一轮轮询内就绪就跳过
        解题；否则解一次验证码再轮询第二轮。异常向上抛，由 scrape 统一
        转成带类名的错误信息。
        """
        logger.info("正在获取网页源码: %s", url)
        if ready is None:
            ready = lambda html: bool(html and html.strip())  # noqa: E731
        with (
            contextlib.redirect_stdout(sys.stderr),
            SB(uc=True, test=True, locale="en", headless=True) as sb,
        ):
            sb.activate_cdp_mode(url)
            html = BaseScraper._wait_ready(sb, ready, PAGE_WAIT_MAX_SEC)
            if not ready(html):
                try:
                    sb.solve_captcha()
                except Exception:
                    # 页面本来就没有验证码时，个别版本会抛错；能取到源码就不算失败
                    logger.debug("solve_captcha 跳过", exc_info=True)
                html = BaseScraper._wait_ready(sb, ready, CAPTCHA_WAIT_MAX_SEC)
            return html

    @staticmethod
    def _wait_ready(sb, ready, budget_sec: float) -> str:
        """按 POLL_INTERVAL_SEC 轮询页面源码，直到就绪或耗尽预算。"""
        html = sb.get_page_source()
        waited = 0.0
        while not ready(html) and waited < budget_sec:
            sb.sleep(POLL_INTERVAL_SEC)
            waited += POLL_INTERVAL_SEC
            html = sb.get_page_source()
        return html

    def page_ready(self, html: str) -> bool:
        """钩子方法：能解析出 m3u8 与片名即认为页面就绪（挑战页解析不出来）。"""
        if not html:
            return False
        try:
            m3u8, name = self.extract(html)
        except Exception:
            return False
        return bool(m3u8 and name)

    @staticmethod
    def extract_group(
        content: str, pattern: str, default: str | None = None
    ) -> str | None:
        """正则提取第一个捕获组。"""
        match = re.search(pattern, content)
        return match.group(1) if match else default

    @staticmethod
    def url_alive(url: str) -> bool:
        """HEAD 探活（标准库实现，替代 requests）。"""
        try:
            req = Request(url, method="HEAD", headers={"User-Agent": UA})
            with urlopen(req, timeout=10) as resp:
                return resp.status == 200
        except OSError:  # URLError / 超时 / 连接失败均视为不可用
            return False

    def fix_m3u8(self, url: str) -> str:
        """钩子方法：子类可修正 m3u8 地址。"""
        return url

    def extra_headers(self, m3u8_url: str) -> dict[str, str]:
        """钩子方法：子类可返回下载所需的额外请求头。"""
        return {}

    def extract_actors(self, html: str) -> list[str]:
        """钩子方法：子类可返回页面上的演员名，用于归档到演员子目录。"""
        return []

    @abstractmethod
    def extract(self, html: str) -> tuple[str | None, str | None]:
        """从 HTML 提取 (m3u8 地址, 视频名)。"""
