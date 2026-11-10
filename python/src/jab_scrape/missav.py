"""MissAV 站点解析。"""

from .scraper import UA, BaseScraper


class MissAVScraper(BaseScraper):
    # 页面内 JSON 转义过的 UUID
    UUID_PATTERN = r"https:\\/\\/surrit\.com\\/([a-f0-9-]{36})\\/seek\\/_0\.jpg"
    URL_TEMPLATE = "https://surrit.com/{uuid}/720p/video.m3u8"

    def extract(self, html: str) -> tuple[str | None, str | None]:
        title = self.extract_group(html, r"<title>(.*?)</title>")
        video_name = title.split(" ")[0] if title else None
        if video_name in ("Jable", "MissAV"):
            video_name = None

        uuid = self.extract_group(html, self.UUID_PATTERN)
        m3u8_url = self.URL_TEMPLATE.format(uuid=uuid) if uuid else None
        return m3u8_url, video_name

    def fix_m3u8(self, url: str) -> str:
        """默认清晰度无效时切换到 1280x720 路径。"""
        if not self.url_alive(url):
            return url.replace("720p", "1280x720")
        return url

    def extra_headers(self, m3u8_url: str) -> dict[str, str]:
        return {
            "referer": m3u8_url,
            "sec-ch-ua": '"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"',
            "sec-ch-ua-mobile": "?0",
            "sec-ch-ua-platform": '"Windows"',
            "user-agent": UA,
        }
