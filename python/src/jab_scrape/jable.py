"""Jable 站点解析。"""

from bs4 import BeautifulSoup

from .scraper import BaseScraper

# 演员全名所在的属性：静态 HTML 里是 title，页面 JS 用 Bootstrap 初始化 tooltip
# 后会把 title 搬到 data-original-title 并把 title 置空，所以两个都要读。
NAME_ATTRS = ("data-original-title", "title")

# 中文字幕标记（两条路，按可靠性排序）：
# 1. 分类标签：站内中文字幕作品会挂一个指向 chinese-subtitle 分类的链接。
#    但这个链接在页面导航栏、推荐位也有，只能在**当前作品的详情区**里找，
#    全页查询会把普通作品误判成字幕版。
# 2. 正文文案：标签被改版时兜底，「中文字幕」这几个字一直在详情描述里。
CHINESE_SUB_CAT = 'a[href*="chinese-subtitle"]'
CHINESE_SUB_TEXT = "中文字幕"

# 详情信息区容器，按页面结构从内到外依次尝试；都没有就只认正文文案。
DETAIL_SCOPES = ("section.video-info", ".video-info", ".info-header")


class JableScraper(BaseScraper):
    def extract(self, html: str) -> tuple[str | None, str | None]:
        m3u8_url = self.extract_group(html, r"var hlsUrl = '(.*?)'")

        title = self.extract_group(html, r"<title>(.*?)</title>")
        if not title:
            return None, None

        video_name = title.split(" ")[0]
        if video_name in ("Jable", "MissAV"):
            return None, None

        if self.has_chinese_subtitle(html):
            video_name += "-C"

        return m3u8_url, video_name

    @staticmethod
    def has_chinese_subtitle(html: str) -> bool:
        """是否中文字幕版：先认详情区里的分类标签，再回退到详情文案。

        标签只在详情区里算数——页面顶部导航、侧边推荐位也挂着同一个分类链接，
        全页匹配会让普通作品被误判成字幕版（进而被改名成 -C、覆盖掉原文件）。
        """
        soup = BeautifulSoup(html, "html.parser")

        for selector in DETAIL_SCOPES:
            scope = soup.select_one(selector)
            if scope is None:
                continue
            if scope.select_one(CHINESE_SUB_CAT):
                return True
            break  # 已经定位到详情区，不再往全页扩

        desc = soup.select_one("h5.desc")
        return bool(desc and CHINESE_SUB_TEXT in desc.get_text())

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
