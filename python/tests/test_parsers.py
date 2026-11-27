"""解析逻辑离线测试：不启动浏览器，不访问网络。

覆盖 jable / missav 的 extract、extra_headers，
以及 BaseScraper 主流程的失败分支。
"""

from __future__ import annotations

import json
from dataclasses import asdict

import pytest

from jab_scrape.jable import JableScraper
from jab_scrape.missav import MissAVScraper
from jab_scrape.models import PageSnapshot, ScrapeResult
from jab_scrape.scraper import BaseScraper

UUID = "123e4567-e89b-12d3-a456-426614174000"
MASTER = f"https://c.surrit.com/{UUID}/playlist.m3u8"
VIDEO = f"https://c.surrit.com/{UUID}/720p/video.m3u8"


def snapshot(html, **kwargs):
    return PageSnapshot(
        html=html,
        **{
            "url": "https://missav.ws/cn/abc-123",
            "user_agent": "browser-test-agent/1.0",
            **kwargs,
        },
    )


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

# 只有分类标签、没有那句文案（标签是首选判据）
JABLE_HTML_CAT_TAG = """
<html><head><title>ABC-123 作品标题</title></head>
<body>
<section class="video-info">
  <div class="info-header">
    <div class="header-right">
      <a class="cat" href="https://jable.tv/categories/chinese-subtitle/">中文字幕</a>
    </div>
  </div>
</section>
<script>var hlsUrl = 'https://cdn.jable.tv/hls/abc-123.m3u8';</script>
</body></html>
"""

# 导航栏/推荐位挂着同一个分类链接，但当前作品不是字幕版：不能误判
JABLE_HTML_NAV_CAT = """
<html><head><title>ABC-123 作品标题</title></head>
<body>
<nav><a href="https://jable.tv/categories/chinese-subtitle/">中文字幕</a></nav>
<section class="video-info">
  <div class="info-header"><h4>ABC-123 作品标题</h4></div>
</section>
<script>var hlsUrl = 'https://cdn.jable.tv/hls/abc-123.m3u8';</script>
</body></html>
"""

MISSAV_HTML = (
    "<html><head><title>ABC-123 作品标题</title></head><body>"
    r'"image": "https:\/\/surrit.com\/' + UUID + r'\/seek\/_0.jpg"'
    "</body></html>"
)

# 演员节点：全名只在 title 里，页面上只显示首字（真实样本见项目根 jable.html）
JABLE_HTML_WITH_ACTORS = """
<html><head><title>ABC-123 作品标题</title></head>
<body>
<div class="models">
  <a class="model" href="https://jable.tv/models/hash1/">
    <span class="placeholder rounded-circle" title="釈アリス">釈</span>
  </a>
  <a class="model" href="https://jable.tv/models/hash2/">
    <span class="placeholder rounded-circle" title="三上悠亜">三</span>
  </a>
</div>
<script>var hlsUrl = 'https://cdn.jable.tv/hls/abc-123.m3u8';</script>
</body></html>
"""

# 页面 JS 初始化 tooltip 之后的形态：title 被搬空，只剩 data-original-title
JABLE_HTML_TOOLTIP = """
<html><head><title>ABC-123 作品标题</title></head>
<body>
<div class="models">
  <a class="model" href="https://jable.tv/models/hash1/">
    <span class="placeholder rounded-circle" data-original-title="釈アリス" title="">釈</span>
  </a>
</div>
<script>var hlsUrl = 'https://cdn.jable.tv/hls/abc-123.m3u8';</script>
</body></html>
"""


class TestJable:
    def test_extract_ok(self):
        m3u8, name = JableScraper().extract(snapshot(JABLE_HTML))
        assert m3u8 == "https://cdn.jable.tv/hls/abc-123.m3u8"
        assert name == "ABC-123"

    def test_chinese_subtitle_suffix(self):
        _, name = JableScraper().extract(snapshot(JABLE_HTML_FIXED))
        assert name == "ABC-123-C"

    def test_chinese_subtitle_by_cat_tag(self):
        """分类标签是首选判据：没有那句文案也要认出来。"""
        _, name = JableScraper().extract(snapshot(JABLE_HTML_CAT_TAG))
        assert name == "ABC-123-C"

    def test_cat_tag_outside_detail_ignored(self):
        """导航栏/推荐位的分类链接不算，只有详情区里的才算。"""
        _, name = JableScraper().extract(snapshot(JABLE_HTML_NAV_CAT))
        assert name == "ABC-123"

    def test_chinese_subtitle_absent(self):
        _, name = JableScraper().extract(snapshot(JABLE_HTML))
        assert name == "ABC-123"

    def test_chinese_subtitle_text_outside_desc_ignored(self):
        """文案判定只看详情描述区，页面别处出现「中文字幕」不算。"""
        html = """
        <html><head><title>ABC-123 作品标题</title></head>
        <body><div class="header-right">中文字幕</div></body></html>
        """
        _, name = JableScraper().extract(snapshot(html))
        assert name == "ABC-123"

    def test_site_title_rejected(self):
        html = "<title>Jable</title>"
        assert JableScraper().extract(snapshot(html)) == (None, None)

    def test_missing_title(self):
        assert JableScraper().extract(snapshot("<html></html>")) == (None, None)

    def test_extract_actors_multiple(self):
        assert JableScraper().extract_actors(JABLE_HTML_WITH_ACTORS) == [
            "釈アリス",
            "三上悠亜",
        ]

    def test_extract_actors_tooltip_attr(self):
        """JS 初始化 tooltip 后 title 被置空，要能读 data-original-title。"""
        assert JableScraper().extract_actors(JABLE_HTML_TOOLTIP) == ["釈アリス"]

    def test_extract_actors_dedup(self):
        html = """
        <div class="models">
          <a class="model" href="/models/h1/"><span title="釈アリス">釈</span></a>
          <a class="model" href="/models/h1/"><span title="釈アリス">釈</span></a>
        </div>
        """
        assert JableScraper().extract_actors(html) == ["釈アリス"]

    def test_extract_actors_absent(self):
        assert JableScraper().extract_actors(JABLE_HTML) == []

    def test_extract_actors_fallback_to_text(self):
        html = '<div class="models"><a class="model">釈</a></div>'
        assert JableScraper().extract_actors(html) == ["釈"]


class TestMissAV:
    def test_master_address_alone_is_not_ready(self):
        page = snapshot(MISSAV_HTML, player_url=MASTER, media_urls=[MASTER])
        assert MissAVScraper().extract(page) == (None, "ABC-123")
        assert not MissAVScraper().page_ready(page)

    def test_only_unrelated_variants_do_not_complete_main_video(self):
        other = VIDEO.replace(UUID, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
        page = snapshot(MISSAV_HTML, player_url=MASTER, variant_urls=[other])
        assert not MissAVScraper().page_ready(page)

    def test_chooses_highest_variant_over_current_playback_quality(self):
        page = snapshot(
            MISSAV_HTML,
            player_url=MASTER,
            variant_urls=[f"https://c.surrit.com/{UUID}/360p/video.m3u8", VIDEO],
            media_urls=[f"https://c.surrit.com/{UUID}/360p/video.m3u8", MASTER],
        )
        assert MissAVScraper().extract(page) == (VIDEO, "ABC-123")

    def test_uses_parsed_variants_when_resource_buffer_is_empty(self):
        assert MissAVScraper().extract(
            snapshot(
                MISSAV_HTML,
                player_url=MASTER,
                variant_urls=[VIDEO],
            )
        ) == (VIDEO, "ABC-123")

    def test_thumbnail_uuid_does_not_invent_a_stream(self):
        assert MissAVScraper().extract(snapshot(MISSAV_HTML)) == (None, "ABC-123")

    def test_title_and_network_do_not_need_thumbnail(self):
        assert MissAVScraper().extract(
            snapshot(
                "<title>ABC-123 x</title>",
                media_urls=[VIDEO],
            )
        ) == (VIDEO, "ABC-123")

    def test_only_observed_variants_choose_highest_and_keep_query(self):
        highest = f"https://c.surrit.com/{UUID}/1080p/video.m3u8?token=a%2Bb&x=1"
        page = snapshot(
            MISSAV_HTML,
            media_urls=[
                f"https://c.surrit.com/{UUID}/480p/video.m3u8",
                highest,
            ],
        )
        assert MissAVScraper().extract(page)[0] == highest

    def test_480p_only_does_not_require_720p(self):
        url = f"https://c.surrit.com/{UUID}/480p/video.m3u8"
        assert (
            MissAVScraper().extract(snapshot(MISSAV_HTML, media_urls=[url]))[0] == url
        )

    def test_ignores_other_video_when_player_identifies_main(self):
        other = VIDEO.replace(UUID, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
        assert (
            MissAVScraper().extract(
                snapshot(
                    MISSAV_HTML,
                    player_url=MASTER,
                    media_urls=[other, VIDEO],
                )
            )[0]
            == VIDEO
        )

    def test_ambiguous_video_ids_are_not_guessed(self):
        other = VIDEO.replace(UUID, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
        assert (
            MissAVScraper().extract(
                snapshot(
                    MISSAV_HTML,
                    media_urls=[other, VIDEO],
                )
            )[0]
            is None
        )

    @pytest.mark.parametrize(
        "url",
        [
            "https://ads.example/playlist.m3u8",
            VIDEO.replace("c.surrit.com", "c.surrit.com.example"),
            VIDEO.replace("https:", "file:"),
            "blob:" + VIDEO,
            "https://[invalid/playlist.m3u8",
        ],
    )
    def test_ignores_unrelated_and_invalid_urls(self, url):
        assert (
            MissAVScraper().extract(snapshot(MISSAV_HTML, media_urls=[url]))[0] is None
        )

    def test_site_title_rejected(self):
        assert (
            MissAVScraper().extract(
                snapshot(
                    "<title>MissAV</title>",
                    media_urls=[MASTER],
                )
            )[1]
            is None
        )

    @pytest.mark.parametrize("origin", ["https://missav.ws", "https://missav.com"])
    def test_scrape_uses_final_page_url_and_actual_browser_agent(
        self, monkeypatch, origin
    ):
        page_url = f"{origin}/cn/abc-123?lang=cn#player"
        page = snapshot(MISSAV_HTML, url=page_url, media_urls=[VIDEO])
        monkeypatch.setattr(BaseScraper, "fetch_page", staticmethod(lambda url: page))
        result = MissAVScraper().scrape("https://missav.example/redirect")
        assert result.ok is True
        assert result.m3u8_url == VIDEO
        assert result.headers == {
            "referer": f"{origin}/cn/abc-123?lang=cn",
            "origin": origin,
            "user-agent": "browser-test-agent/1.0",
        }

    def test_missing_browser_agent_reports_failure(self, monkeypatch):
        page = snapshot(MISSAV_HTML, user_agent="", media_urls=[VIDEO])
        monkeypatch.setattr(BaseScraper, "fetch_page", staticmethod(lambda url: page))
        result = MissAVScraper().scrape(page.url)
        assert not result.ok
        assert "User-Agent" in result.error


class TestBaseScraper:
    def test_extract_group_default(self):
        assert BaseScraper.extract_group("abc", r"(x)", default=None) is None
        assert BaseScraper.extract_group("abc", r"(b)") == "b"

    def test_scrape_fetch_failed(self, monkeypatch):
        monkeypatch.setattr(
            BaseScraper, "fetch_page", staticmethod(lambda url: snapshot(""))
        )
        result = JableScraper().scrape("https://example.com")
        assert result.ok is False
        assert result.error == "获取网页源码失败"

    def test_scrape_extract_raises(self, monkeypatch):
        class Boom(JableScraper):
            def extract(self, html):
                raise ValueError("boom")

        monkeypatch.setattr(
            BaseScraper, "fetch_page", staticmethod(lambda url: snapshot("<html/>"))
        )
        result = Boom().scrape("https://example.com")
        assert result.ok is False
        assert "boom" in result.error

    def test_scrape_empty_fields(self, monkeypatch):
        monkeypatch.setattr(
            BaseScraper, "fetch_page", staticmethod(lambda url: snapshot("<html/>"))
        )
        result = JableScraper().scrape("https://example.com")
        assert result.ok is False
        assert "未能获取有效信息" in result.error

    def test_scrape_ok(self, monkeypatch):
        monkeypatch.setattr(
            BaseScraper, "fetch_page", staticmethod(lambda url: snapshot(JABLE_HTML))
        )
        result = JableScraper().scrape("https://example.com")
        assert result.ok is True
        assert result.video_name == "ABC-123"
        assert result.headers == {}
        assert result.actors == []

    def test_scrape_carries_actors(self, monkeypatch):
        monkeypatch.setattr(
            BaseScraper,
            "fetch_page",
            staticmethod(lambda url: snapshot(JABLE_HTML_WITH_ACTORS)),
        )
        result = JableScraper().scrape("https://example.com")
        assert result.ok is True
        assert result.actors == ["釈アリス", "三上悠亜"]

    def test_actor_failure_does_not_break_scrape(self, monkeypatch):
        """演员名只用于归档，解析炸了也不能让整次下载失败。"""

        class Boom(JableScraper):
            def extract_actors(self, html):
                raise ValueError("boom")

        monkeypatch.setattr(
            BaseScraper, "fetch_page", staticmethod(lambda url: snapshot(JABLE_HTML))
        )
        result = Boom().scrape("https://example.com")
        assert result.ok is True
        assert result.actors == []


class TestScrapeResult:
    def test_to_dict_shape(self):
        d = ScrapeResult(ok=True, m3u8_url="u", video_name="n").to_dict()
        assert d == {
            "ok": True,
            "m3u8_url": "u",
            "video_name": "n",
            "headers": {},
            "actors": [],
            "error": None,
        }

    def test_defaults(self):
        d = ScrapeResult(ok=False, error="e").to_dict()
        assert d["headers"] == {}
        assert d["m3u8_url"] is None
        assert d["actors"] == []


class TestFetchPageStdoutIsolation:
    def test_sb_noise_goes_to_stderr(self, monkeypatch, capsys):
        """SB 的 print 噪声不能污染 stdout（Go 侧按单行 JSON 解析）。"""
        from jab_scrape import scraper as mod

        class FakeSB:
            def __init__(self, **kwargs):
                self.cdp = self

            def evaluate(self, script):
                return json.dumps(asdict(snapshot(self.get_page_source())))

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
                return JABLE_HTML

        monkeypatch.setattr(mod, "SB", FakeSB)
        page = JableScraper().fetch_page("https://example.com")
        captured = capsys.readouterr()
        assert page.html == JABLE_HTML
        assert captured.out == ""
        assert "starts" in captured.err


class TestFetchPageTiming:
    """条件轮询：就绪即走，不再死等固定秒数；超时才解题再等第二轮。"""

    @staticmethod
    def _fake_sb(calls, sources, captcha=None):
        class FakeSB:
            def __init__(self, **kwargs):
                self._i = 0
                self.cdp = self

            def evaluate(self, script):
                return json.dumps(asdict(snapshot(self.get_page_source())))

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

            def activate_cdp_mode(self, url):
                calls.append(("open", url))

            def sleep(self, seconds):
                calls.append(("sleep", seconds))

            def solve_captcha(self):
                calls.append(("captcha", None))
                if captcha is not None:
                    captcha()

            def get_page_source(self):
                i = min(self._i, len(sources) - 1)
                self._i += 1
                return sources[i]

        return FakeSB

    def test_ready_page_skips_captcha_and_extra_wait(self, monkeypatch):
        """页面一打开就就绪：不解题、不 sleep，直接拿源码走人。"""
        from jab_scrape import scraper as mod

        calls = []
        FakeSB = self._fake_sb(calls, [JABLE_HTML])
        monkeypatch.setattr(mod, "SB", FakeSB)
        page = JableScraper().fetch_page("https://example.com")
        assert page.html == JABLE_HTML
        assert calls == [("open", "https://example.com")]

    def test_polls_until_ready(self, monkeypatch):
        """挑战页要轮询到真页面出现为止，期间按固定间隔小睡。"""
        from jab_scrape import scraper as mod

        calls = []
        FakeSB = self._fake_sb(calls, ["<html>challenge</html>"] * 3 + [JABLE_HTML])
        monkeypatch.setattr(mod, "SB", FakeSB)
        page = JableScraper().fetch_page("https://example.com")
        assert page.html == JABLE_HTML
        assert calls == [
            ("open", "https://example.com"),
            ("sleep", mod.POLL_INTERVAL_SEC),
            ("sleep", mod.POLL_INTERVAL_SEC),
            ("sleep", mod.POLL_INTERVAL_SEC),
        ]

    def test_missav_waits_for_media_after_html_is_ready(self, monkeypatch):
        from jab_scrape import scraper as mod

        calls = []
        pages = [
            snapshot(MISSAV_HTML),
            snapshot(MISSAV_HTML),
            snapshot(
                MISSAV_HTML,
                player_url=MASTER,
                media_urls=[MASTER],
                variant_urls=[VIDEO],
            ),
        ]

        class FakeSB(self._fake_sb(calls, [])):
            def evaluate(self, script):
                page = pages[min(self._i, len(pages) - 1)]
                self._i += 1
                return json.dumps(asdict(page))

        monkeypatch.setattr(mod, "SB", FakeSB)
        result = MissAVScraper().scrape("https://missav.ws/cn/abc-123")
        assert result.ok
        assert result.m3u8_url == VIDEO
        assert calls == [
            ("open", "https://missav.ws/cn/abc-123"),
            ("sleep", mod.POLL_INTERVAL_SEC),
            ("sleep", mod.POLL_INTERVAL_SEC),
        ]

    def test_empty_document_during_navigation_is_polled(self, monkeypatch):
        from jab_scrape import scraper as mod

        calls = []
        monkeypatch.setattr(mod, "SB", self._fake_sb(calls, ["", "", JABLE_HTML]))
        result = JableScraper().scrape("https://example.com")
        assert result.ok
        assert result.video_name == "ABC-123"
        assert calls.count(("sleep", mod.POLL_INTERVAL_SEC)) == 2
        assert not any(call[0] == "captcha" for call in calls)

    def test_browser_exception_is_reported_as_json_failure(self, monkeypatch, capsys):
        from jab_scrape import __main__ as cli
        from jab_scrape import scraper as mod

        class FailingSB(self._fake_sb([], [])):
            def __init__(self, **kwargs):
                super().__init__(**kwargs)
                # 与 SeleniumBase 的 test 模式一致：默认会吞掉异常。
                self.raise_failure = kwargs.get("raise_test_failure", False)

            def evaluate(self, script):
                raise RuntimeError("CDP connection closed")

            def __exit__(self, *exc):
                return not self.raise_failure

        monkeypatch.setattr(mod, "SB", FailingSB)
        assert cli.main(["https://example.com", "--source", "missav"]) == 1
        result = json.loads(capsys.readouterr().out)
        assert result["ok"] is False
        assert "RuntimeError: CDP connection closed" in result["error"]
        assert "NoneType" not in result["error"]

    def test_missing_snapshot_from_context_manager_is_explicit_failure(
        self, monkeypatch
    ):
        from jab_scrape import scraper as mod

        class SwallowingSB(self._fake_sb([], [])):
            def evaluate(self, script):
                raise RuntimeError("unexpected suppressed error")

            def __exit__(self, *exc):
                return True

        monkeypatch.setattr(mod, "SB", SwallowingSB)
        result = MissAVScraper().scrape("https://example.com")
        assert not result.ok
        assert "浏览器未返回页面快照" in result.error

    def test_captcha_only_after_first_window_exhausted(self, monkeypatch):
        """第一轮等满上限仍不就绪才解题；解题后就绪立刻返回。"""
        from jab_scrape import scraper as mod

        calls = []
        state = {"solved": False}

        def solve():
            state["solved"] = True

        class FakeSB(self._fake_sb(calls, [], captcha=solve)):
            def get_page_source(self):
                return JABLE_HTML if state["solved"] else "<html>challenge</html>"

        monkeypatch.setattr(mod, "SB", FakeSB)
        page = JableScraper().fetch_page("https://example.com")
        assert page.html == JABLE_HTML
        first_round_sleeps = int(mod.PAGE_WAIT_MAX_SEC / mod.POLL_INTERVAL_SEC)
        assert calls == (
            [("open", "https://example.com")]
            + [("sleep", mod.POLL_INTERVAL_SEC)] * first_round_sleeps
            + [("captcha", None)]
        )

    def test_gives_up_after_both_windows(self, monkeypatch):
        """两轮都等满仍不就绪：把最后的源码交出去，由解析层报清晰错误。"""
        from jab_scrape import scraper as mod

        calls = []
        FakeSB = self._fake_sb(calls, ["<html>challenge</html>"])
        monkeypatch.setattr(mod, "SB", FakeSB)
        page = JableScraper().fetch_page("https://example.com")
        assert page.html == "<html>challenge</html>"
        total = int(
            (mod.PAGE_WAIT_MAX_SEC + mod.CAPTCHA_WAIT_MAX_SEC) / mod.POLL_INTERVAL_SEC
        )
        assert calls.count(("sleep", mod.POLL_INTERVAL_SEC)) == total
        assert calls.count(("captcha", None)) == 1

    def test_captcha_error_is_tolerated(self, monkeypatch):
        """没有验证码时个别版本 solve_captcha 会抛错，不能因此拿不到源码。"""
        from jab_scrape import scraper as mod

        calls = []
        state = {"tried": False}

        def boom():
            state["tried"] = True
            raise RuntimeError("no captcha")

        class FakeSB(self._fake_sb(calls, [], captcha=boom)):
            def get_page_source(self):
                return JABLE_HTML if state["tried"] else "<html>challenge</html>"

        monkeypatch.setattr(mod, "SB", FakeSB)
        assert JableScraper().fetch_page("https://example.com").html == JABLE_HTML


class TestPageReady:
    def test_ready_on_parseable_page(self):
        assert JableScraper().page_ready(snapshot(JABLE_HTML)) is True

    def test_not_ready_on_challenge_page(self):
        assert JableScraper().page_ready(snapshot("<html>challenge</html>")) is False
        assert JableScraper().page_ready(snapshot("")) is False

    def test_extract_error_means_not_ready(self):
        class Boom(JableScraper):
            def extract(self, html):
                raise ValueError("boom")

        assert Boom().page_ready(snapshot(JABLE_HTML)) is False


class TestScrapeErrorPropagation:
    def test_fetch_exception_carries_class_and_message(self, monkeypatch):
        """浏览器/驱动炸了时，错误里要能看到异常类名，而不是只有一句「失败」。"""

        def boom(url, ready=None):
            raise RuntimeError("driver crashed")

        monkeypatch.setattr(BaseScraper, "fetch_page", staticmethod(boom))
        result = JableScraper().scrape("https://example.com")
        assert result.ok is False
        assert "RuntimeError" in result.error
        assert "driver crashed" in result.error


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
