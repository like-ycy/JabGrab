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

# 页面打开后先等 10 秒再解题：jable/missav 都有 Cloudflare 验证，页面脚本要跑
# 一会儿才把挑战过掉，等太短拿到的还是挑战页（真机上是这条链路最容易失败的地方）。
PAGE_WAIT_SEC = 10
# 解题后再等 5 秒，让标题、演员等 DOM 由页面 JS 渲染完再取源码。
CAPTCHA_WAIT_SEC = 5


class BaseScraper(ABC):
    def scrape(self, url: str) -> ScrapeResult:
        """主流程：抓取 -> 解析 -> 修正 -> 组装结果。"""
        html = self.fetch_html(url)
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
    def fetch_html(url: str) -> str | None:
        """seleniumbase CDP 模式获取页面源码。

        SB 会把启动横幅、驱动下载进度等直接 print 到 stdout，
        这里整体重定向到 stderr，保证 stdout 只有最终那一行 JSON。
        """
        logger.info("正在获取网页源码: %s", url)
        try:
            with (
                contextlib.redirect_stdout(sys.stderr),
                SB(uc=True, test=True, locale="en", headless=True) as sb,
            ):
                sb.activate_cdp_mode(url)
                sb.sleep(PAGE_WAIT_SEC)
                try:
                    sb.solve_captcha()
                except Exception:
                    # 页面本来就没有验证码时，个别版本会抛错；能取到源码就不算失败
                    logger.debug("solve_captcha 跳过", exc_info=True)
                sb.sleep(CAPTCHA_WAIT_SEC)
                return sb.get_page_source()
        except Exception:
            logger.exception("Selenium 获取 HTML 失败")
            return None

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
