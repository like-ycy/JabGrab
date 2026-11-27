"""MissAV 站点解析：使用播放器和网络请求中的实际清单，不猜测画质路径。"""

import re
from urllib.parse import urlsplit

from .models import PageSnapshot
from .scraper import BaseScraper

STREAM_PATH = re.compile(
    r"/([a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12})/"
    r"(?:playlist|([1-9][0-9]*)p/video)\.m3u8",
    re.IGNORECASE,
)


def _stream_key(url: str) -> tuple[str, int] | None:
    """仅识别 MissAV 媒体 CDN；返回作品 UUID 与画质，主清单记为 0。"""
    try:
        parts = urlsplit(url)
    except ValueError:
        return None
    host = parts.hostname or ""
    if parts.scheme not in ("http", "https") or not (
        host == "surrit.com" or host.endswith(".surrit.com")
    ):
        return None
    match = STREAM_PATH.fullmatch(parts.path)
    if not match:
        return None
    return match[1].lower(), int(match[2] or 0)


class MissAVScraper(BaseScraper):
    def extract(self, page: PageSnapshot) -> tuple[str | None, str | None]:
        title = self.extract_group(page.html, r"<title>(.*?)</title>")
        video_name = title.split(" ")[0] if title else None
        if video_name in ("Jable", "MissAV"):
            video_name = None

        candidates = {
            url: key
            for url in [*page.variant_urls, *page.media_urls]
            if (key := _stream_key(url)) is not None and key[1] > 0
        }
        active = _stream_key(page.player_url)
        video_ids = {key[0] for key in candidates.values()}
        # 页面可能同时加载推荐视频；优先定位当前播放器，无法消歧时继续等待。
        if not active and len(video_ids) != 1:
            return None, video_name
        video_id = active[0] if active else next(iter(video_ids))
        urls = [url for url, key in candidates.items() if key[0] == video_id]
        if not urls:
            # hls.url 在请求主清单之前就已设置，不能据此认为媒体解析成功。
            return None, video_name
        # 画质由浏览器实际解析的列表决定；下载器直接读取分片清单，
        # 与已验证的手动下载流程一致，不再重新请求主清单或拼出固定 720p。
        selected = max(
            urls,
            key=lambda url: (
                candidates[url][1],
                url in page.variant_urls,
            ),
        )
        return selected, video_name

    def extra_headers(self, page: PageSnapshot) -> dict[str, str]:
        source = urlsplit(page.url)
        if (
            source.scheme not in ("http", "https")
            or not source.netloc
            or not page.user_agent
        ):
            raise ValueError("未获取到有效的 MissAV 来源页面或浏览器 User-Agent")
        return {
            "referer": source._replace(fragment="").geturl(),
            "origin": f"{source.scheme}://{source.netloc}",
            "user-agent": page.user_agent,
        }
