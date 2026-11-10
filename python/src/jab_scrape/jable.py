"""Jable 站点解析。"""

from .scraper import BaseScraper


class JableScraper(BaseScraper):
    def extract(self, html: str) -> tuple[str | None, str | None]:
        m3u8_url = self.extract_group(html, r"var hlsUrl = '(.*?)'")

        title = self.extract_group(html, r"<title>(.*?)</title>")
        if not title:
            return None, None

        video_name = title.split(" ")[0]
        if video_name in ("Jable", "MissAV"):
            return None, None

        # 中文字幕版标记
        desc = self.extract_group(html, r'<h5 class="desc h6-md">([^<]*)</h5>')
        if desc == "此作品曾在本站上傳，現已更新至中文字幕版。":
            video_name += "-C"

        return m3u8_url, video_name
