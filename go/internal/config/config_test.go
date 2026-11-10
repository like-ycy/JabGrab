package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchSite(t *testing.T) {
	cfg := Default()

	cases := []struct {
		url  string
		site string
		ok   bool
	}{
		{"https://jable.tv/videos/cjod-548/", "jable", true},
		{"https://JABLE.tv/x", "jable", true}, // 大小写不敏感
		{"https://missav.com/en/abc", "missav", true},
		{"https://example.com/a", "", false},
	}
	for _, c := range cases {
		site, ok := cfg.MatchSite(c.url)
		if ok != c.ok || site != c.site {
			t.Fatalf("MatchSite(%q)=(%q,%v) want (%q,%v)", c.url, site, ok, c.site, c.ok)
		}
	}
}

func TestMatchSiteIgnoresDisabled(t *testing.T) {
	cfg := Default()
	cfg.Sites["missav"] = Site{Enabled: false, Keyword: "missav"}
	if _, ok := cfg.MatchSite("https://missav.com/x"); ok {
		t.Fatal("禁用的站点不应命中")
	}
}

// 关键字重叠时取更具体的（更长的）那个。
func TestMatchSitePrefersLongerKeyword(t *testing.T) {
	cfg := Default()
	cfg.Sites["mirror"] = Site{Enabled: true, Keyword: "missav.ws"}
	site, ok := cfg.MatchSite("https://missav.ws/en/abc")
	if !ok || site != "mirror" {
		t.Fatalf("应命中更具体的关键字 got=%q ok=%v", site, ok)
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	c := Config{} // 全零值，模拟旧配置缺字段
	c.normalize()

	if c.SaveDir == "" {
		t.Fatal("保存目录应补默认值")
	}
	if c.Download.Threads <= 0 || c.Download.RetryCount <= 0 || c.Download.TimeoutSec <= 0 {
		t.Fatalf("下载参数应补默认值: %+v", c.Download)
	}
	if c.ScrapeTimeoutSec <= 0 {
		t.Fatal("爬取超时应补默认值")
	}
	if _, ok := c.Sites["jable"]; !ok {
		t.Fatal("应补上默认站点")
	}
	if c.RecentTxt == nil {
		t.Fatal("最近列表不应为 nil")
	}
}

// 用户自定义的关键字不能被 normalize 覆盖。
func TestNormalizeKeepsCustomKeyword(t *testing.T) {
	c := Default()
	c.Sites["missav"] = Site{Enabled: true, Keyword: "missav.ws"}
	c.normalize()
	if got := c.Sites["missav"].Keyword; got != "missav.ws" {
		t.Fatalf("自定义关键字被覆盖为 %q", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JABGRAB_CONFIG_DIR", dir)

	cfg := Default()
	cfg.SaveDir = filepath.Join(dir, "videos")
	cfg.TxtPath = filepath.Join(dir, "urls.txt")
	cfg.Sites["jable"] = Site{Enabled: false, Keyword: "jable.tv"}
	cfg.Download.Threads = 8
	if _, err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("配置文件应写入 %s: %v", dir, err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.SaveDir != cfg.SaveDir || got.TxtPath != cfg.TxtPath {
		t.Fatalf("路径未还原: %+v", got)
	}
	if got.Sites["jable"].Enabled || got.Sites["jable"].Keyword != "jable.tv" {
		t.Fatalf("站点设置未还原: %+v", got.Sites["jable"])
	}
	if got.Download.Threads != 8 {
		t.Fatalf("线程数未还原: %d", got.Download.Threads)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("JABGRAB_CONFIG_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("配置不存在时不应报错: %v", err)
	}
	if cfg.SaveDir == "" {
		t.Fatal("应返回带默认值的配置")
	}
}

func TestPushRecentTxt(t *testing.T) {
	c := Default()
	c.PushRecentTxt("/a.txt")
	c.PushRecentTxt("/b.txt")
	c.PushRecentTxt("/a.txt")
	if len(c.RecentTxt) != 2 || c.RecentTxt[0] != "/a.txt" {
		t.Fatalf("最近列表应为 [/a.txt /b.txt]，实际 %v", c.RecentTxt)
	}
	c.PushRecentTxt("")
	if len(c.RecentTxt) != 2 {
		t.Fatal("空路径不应入列表")
	}
}

func TestCheckReportsProblems(t *testing.T) {
	cfg := Default()
	cfg.TxtPath = ""
	cfg.DlrPath = "/definitely/not/here"
	cfg.Sites["jable"] = Site{Enabled: false, Keyword: "jable"}
	cfg.Sites["missav"] = Site{Enabled: false, Keyword: "missav"}

	issues := cfg.Check()
	if len(issues) < 3 {
		t.Fatalf("应报出多个问题，实际: %v", issues)
	}
}

func TestCheckOK(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "u.txt")
	if err := os.WriteFile(txt, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.SaveDir = filepath.Join(dir, "out")
	cfg.TxtPath = txt

	if issues := cfg.Check(); len(issues) != 0 {
		t.Fatalf("不该有问题: %v", issues)
	}
}

func TestSiteNamesSorted(t *testing.T) {
	cfg := Default()
	names := cfg.SiteNames()
	if len(names) != 2 || names[0] != "jable" || names[1] != "missav" {
		t.Fatalf("站点顺序应稳定: %v", names)
	}
}

// Save 必须把补全默认值后的配置返回给调用方，
// 否则出现「盘上已修正、内存里还是空」的错位。
func TestSaveReturnsNormalizedConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JABGRAB_CONFIG_DIR", dir)

	cfg := Default()
	cfg.SaveDir = "" // 用户清空了保存目录
	cfg.TxtPath = ""

	saved, err := Save(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SaveDir == "" {
		t.Fatal("返回值应补全默认保存目录")
	}
	if saved.SaveDir != Default().SaveDir {
		t.Fatalf("保存目录应回落默认值，得到 %q", saved.SaveDir)
	}
	// 传进去的原值不应被改动（值语义），提醒调用方必须用返回值
	if cfg.SaveDir != "" {
		t.Fatalf("不应修改调用方的原值，实际变成 %q", cfg.SaveDir)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JABGRAB_CONFIG_DIR", dir)

	if Exists() {
		t.Fatal("空目录下不该认为配置已存在")
	}
	if _, err := Save(Default()); err != nil {
		t.Fatal(err)
	}
	if !Exists() {
		t.Fatal("写入后应检测到配置存在")
	}
}

// 手改坏的配置不能被默认值覆盖：Load 报错时文件必须原样保留。
func TestLoadKeepsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JABGRAB_CONFIG_DIR", dir)

	broken := []byte("{ 这不是合法 JSON ")
	p := filepath.Join(dir, fileName)
	if err := os.WriteFile(p, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("损坏的配置应当报错")
	}
	if !Exists() {
		t.Fatal("文件仍在，Exists 应为 true")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(broken) {
		t.Fatalf("原文件被改动了: %q", got)
	}
}
