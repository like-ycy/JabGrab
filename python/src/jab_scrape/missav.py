"""MissAV 站点解析。"""

from urllib.parse import urlsplit

from .scraper import UA, BaseScraper


class MissAVScraper(BaseScraper):
    # 页面内 JSON 转义过的 UUID
    UUID_PATTERN = r"https:\\/\\/surrit\.com\\/([a-f0-9-]{36})\\/seek\\/_0\.jpg"
    URL_TEMPLATE = "https://c.surrit.com/{uuid}/720p/video.m3u8"

    def extract(self, html: str) -> tuple[str | None, str | None]:
        title = self.extract_group(html, r"<title>(.*?)</title>")
        video_name = title.split(" ")[0] if title else None
        if video_name in ("Jable", "MissAV"):
            video_name = None

        uuid = self.extract_group(html, self.UUID_PATTERN)
        m3u8_url = self.URL_TEMPLATE.format(uuid=uuid) if uuid else None
        return m3u8_url, video_name

    def extra_headers(self, page_url: str) -> dict[str, str]:
        # 防盗链校验的是来源网页，不能把媒体清单自身当作 Referer。
        page = urlsplit(page_url)
        return {
            "referer": page._replace(fragment="").geturl(),
            "origin": f"{page.scheme}://{page.netloc}",
            "user-agent": UA,
        }
