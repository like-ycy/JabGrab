"""解析逻辑离线测试：不启动浏览器，不访问网络。

覆盖 jable / missav 的 extract、fix_m3u8、extra_headers，
以及 BaseScraper 主流程的失败分支。
"""

from __future__ import annotations

import pytest

from jab_scrape.jable import JableScraper
from jab_scrape.missav import MissAVScraper
from jab_scrape.models import ScrapeResult
from jab_scrape.scraper import BaseScraper

UUID = "123e4567-e89b-12d3-a456-426614174000"

JABLE_HTML = """
<html><head><title>ABC-123 作品标题</title></head>
<body><script>var hlsUrl = 'https://cdn.jable.tv/hls/abc-123.m3u8';</script></body>
</html>
"""

JABLE_HTML_FIXED = """
<html><head><title>ABC-123 作品标题</title></head>
<body>
<h5 class="desc h6-md">此作品曾在本站上傳，現已更新至中文字幕版。</h5>
<script>var hlsUrl = 'https://cdn.jable.tv/hls/abc-123.m3u8';</script>
</body></html>
"""

MISSAV_HTML = (
    "<html><head><title>ABC-123 作品标题</title></head><body>"
    r'"image": "https:\/\/surrit.com\/' + UUID + r'\/seek\/_0.jpg"'
    "</body></html>"
)


class TestJable:
    def test_extract_ok(self):
        m3u8, name = JableScraper().extract(JABLE_HTML)
        assert m3u8 == "https://cdn.jable.tv/hls/abc-123.m3u8"
        assert name == "ABC-123"

    def test_chinese_subtitle_suffix(self):
        _, name = JableScraper().extract(JABLE_HTML_FIXED)
        assert name == "ABC-123-C"

    def test_site_title_rejected(self):
        html = "<title>Jable</title>"
        assert JableScraper().extract(html) == (None, None)

    def test_missing_title(self):
        assert JableScraper().extract("<html></html>") == (None, None)


class TestMissAV:
    def test_extract_ok(self):
        m3u8, name = MissAVScraper().extract(MISSAV_HTML)
        assert m3u8 == f"https://surrit.com/{UUID}/720p/video.m3u8"
        assert name == "ABC-123"

    def test_no_uuid(self):
        m3u8, name = MissAVScraper().extract("<title>ABC-123 x</title>")
        assert m3u8 is None
        assert name == "ABC-123"

    def test_site_title_rejected(self):
        html = r'<title>MissAV</title>"https:\/\/surrit.com\/' + UUID
        _, name = MissAVScraper().extract(html)
        assert name is None

    def test_fix_m3u8_fallback(self, monkeypatch):
        monkeypatch.setattr(BaseScraper, "url_alive", staticmethod(lambda url: False))
        url = MissAVScraper().fix_m3u8(f"https://surrit.com/{UUID}/720p/video.m3u8")
        assert url == f"https://surrit.com/{UUID}/1280x720/video.m3u8"

    def test_fix_m3u8_keeps_alive_url(self, monkeypatch):
        monkeypatch.setattr(BaseScraper, "url_alive", staticmethod(lambda url: True))
        url = f"https://surrit.com/{UUID}/720p/video.m3u8"
        assert MissAVScraper().fix_m3u8(url) == url

    def test_extra_headers(self):
        url = f"https://surrit.com/{UUID}/720p/video.m3u8"
        headers = MissAVScraper().extra_headers(url)
        assert headers["referer"] == url
        assert "user-agent" in headers


class TestBaseScraper:
    def test_extract_group_default(self):
        assert BaseScraper.extract_group("abc", r"(x)", default=None) is None
        assert BaseScraper.extract_group("abc", r"(b)") == "b"

    def test_scrape_fetch_failed(self, monkeypatch):
        monkeypatch.setattr(BaseScraper, "fetch_html", staticmethod(lambda url: ""))
        result = JableScraper().scrape("https://example.com")
        assert result.ok is False
        assert result.error == "获取网页源码失败"

    def test_scrape_extract_raises(self, monkeypatch):
        class Boom(JableScraper):
            def extract(self, html):
                raise ValueError("boom")

        monkeypatch.setattr(
            BaseScraper, "fetch_html", staticmethod(lambda url: "<html/>")
        )
        result = Boom().scrape("https://example.com")
        assert result.ok is False
        assert "boom" in result.error

    def test_scrape_empty_fields(self, monkeypatch):
        monkeypatch.setattr(
            BaseScraper, "fetch_html", staticmethod(lambda url: "<html/>")
        )
        result = JableScraper().scrape("https://example.com")
        assert result.ok is False
        assert "未能获取有效信息" in result.error

    def test_scrape_ok(self, monkeypatch):
        monkeypatch.setattr(
            BaseScraper, "fetch_html", staticmethod(lambda url: JABLE_HTML)
        )
        result = JableScraper().scrape("https://example.com")
        assert result.ok is True
        assert result.video_name == "ABC-123"
        assert result.headers == {}


class TestScrapeResult:
    def test_to_dict_shape(self):
        d = ScrapeResult(ok=True, m3u8_url="u", video_name="n").to_dict()
        assert d == {
            "ok": True,
            "m3u8_url": "u",
            "video_name": "n",
            "headers": {},
            "error": None,
        }

    def test_defaults(self):
        d = ScrapeResult(ok=False, error="e").to_dict()
        assert d["headers"] == {}
        assert d["m3u8_url"] is None


class TestFetchHtmlStdoutIsolation:
    def test_sb_noise_goes_to_stderr(self, monkeypatch, capsys):
        """SB 的 print 噪声不能污染 stdout（Go 侧按单行 JSON 解析）。"""
        from jab_scrape import scraper as mod

        class FakeSB:
            def __init__(self, **kwargs):
                pass

            def __enter__(self):
                print("{jab-scrape:10:SB} starts")
                return self

            def __exit__(self, *exc):
                return False

            def activate_cdp_mode(self, url):
                print("activating cdp")

            def sleep(self, seconds):
                pass

            def solve_captcha(self):
                print("captcha ok")

            def get_page_source(self):
                return "<html>ok</html>"

        monkeypatch.setattr(mod, "SB", FakeSB)
        html = mod.BaseScraper.fetch_html("https://example.com")
        captured = capsys.readouterr()
        assert html == "<html>ok</html>"
        assert captured.out == ""
        assert "starts" in captured.err


@pytest.mark.parametrize("source", ["jable", "missav"])
def test_cli_exit_code_and_stdout(source, monkeypatch, capsys):
    """CLI 契约：成功 exit 0 + stdout 单行 JSON；失败 exit 1。"""
    from jab_scrape import __main__ as cli

    class Fake:
        def scrape(self, url):
            return ScrapeResult(ok=False, error="stub")

    monkeypatch.setitem(cli.SCRAPERS, source, Fake)
    code = cli.main(["https://example.com", "--source", source])
    out = capsys.readouterr().out.strip()
    assert code == 1
    assert out.startswith("{") and '"ok": false' in out


def test_verbose_still_silences_third_party_loggers(monkeypatch):
    """-v 只放大本项目日志，第三方库保持 WARNING 免得刷屏。"""
    import logging

    from jab_scrape import __main__ as cli

    class Fake:
        def scrape(self, url):
            return ScrapeResult(ok=True, m3u8_url="u", video_name="n")

    monkeypatch.setitem(cli.SCRAPERS, "jable", Fake)
    cli.main(["https://example.com", "--source", "jable", "-v"])
    for name in cli.NOISY_LOGGERS:
        assert logging.getLogger(name).level == logging.WARNING
