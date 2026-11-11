"""Jable 站点解析。"""

from bs4 import BeautifulSoup

from .scraper import BaseScraper

# 演员全名所在的属性：静态 HTML 里是 title，页面 JS 用 Bootstrap 初始化 tooltip
# 后会把 title 搬到 data-original-title 并把 title 置空，所以两个都要读。
NAME_ATTRS = ("data-original-title", "title")


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

    def extract_actors(self, html: str) -> list[str]:
        """详情页的演员名。

        页面结构（可能多个 / 可能没有）：

            <div class="models">
              <a class="model" href="https://jable.tv/models/<hash>/">
                <span class="placeholder rounded-circle" title="演员全名">首</span>
              </a>
            </div>
        """
        soup = BeautifulSoup(html, "html.parser")
        actors: list[str] = []
        for anchor in soup.select("div.models a.model"):
            name = _model_name(anchor)
            if name and name not in actors:
                actors.append(name)
        return actors


def _model_name(anchor) -> str:
    """取演员全名：页面上只显示首字，全名藏在 title / data-original-title 里。"""
    for node in [anchor, *anchor.find_all(True)]:
        for attr in NAME_ATTRS:
            value = node.get(attr)
            if isinstance(value, str) and value.strip():
                return value.strip()

    img = anchor.find("img")
    alt = img.get("alt") if img else None
    if isinstance(alt, str) and alt.strip():
        return alt.strip()

    return anchor.get_text(strip=True)
