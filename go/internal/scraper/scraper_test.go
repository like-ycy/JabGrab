package scraper

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseStdout(t *testing.T) {
	out := []byte(`2026-11-08 14:23:35 INFO 正在获取网页源码
{"ok": true, "m3u8_url": "https://cdn/x.m3u8", "video_name": "CJOD-548-C", "headers": {"referer": "https://r"}, "error": null}

`)
	res, err := ParseStdout(out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.M3U8URL != "https://cdn/x.m3u8" || res.VideoName != "CJOD-548-C" {
		t.Fatalf("解析结果不符: %+v", res)
	}
	if res.Headers["referer"] != "https://r" {
		t.Fatalf("headers 未解析: %+v", res.Headers)
	}
}

// Python 侧新增的 actors 字段：有则原样带过来，无则保持空。
func TestParseStdoutActors(t *testing.T) {
	res, err := ParseStdout([]byte(
		`{"ok": true, "m3u8_url": "u", "video_name": "ABC-123", "headers": {}, "actors": ["釈アリス", "三上悠亜"], "error": null}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Actors) != 2 || res.Actors[0] != "釈アリス" || res.Actors[1] != "三上悠亜" {
		t.Fatalf("演员未解析: %+v", res.Actors)
	}

	// 老版本 Python 没有 actors 字段，不能因此报错
	res, err = ParseStdout([]byte(`{"ok": true, "m3u8_url": "u", "video_name": "ABC-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Actors) != 0 {
		t.Fatalf("无 actors 字段时应为空: %+v", res.Actors)
	}
}

func TestParseStdoutError(t *testing.T) {
	res, err := ParseStdout([]byte(`{"ok": false, "error": "获取网页源码失败"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error != "获取网页源码失败" {
		t.Fatalf("失败结果不符: %+v", res)
	}
}

func TestParseStdoutNoJSON(t *testing.T) {
	if _, err := ParseStdout([]byte("只有日志\n没有 JSON\n")); err == nil {
		t.Fatal("没有 JSON 时应报错")
	}
	// 花括号但不是合法 JSON 的行要跳过
	if _, err := ParseStdout([]byte("{不是 JSON}\n")); err == nil {
		t.Fatal("非法 JSON 应被跳过并最终报错")
	}
}

func TestScrapeRunsCommand(t *testing.T) {
	s := Scraper{
		Argv:    []string{"/bin/sh", "-c", `echo '{"ok": true, "m3u8_url": "u", "video_name": "n"}'`},
		Timeout: 10 * time.Second,
	}
	res, err := s.Scrape(context.Background(), "https://example.com", "jable")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.M3U8URL != "u" || res.VideoName != "n" {
		t.Fatalf("结果不符: %+v", res)
	}
}

// 裸命令名（不带路径分隔符）必须仍走 PATH 查找，
// 不能被「相对路径转绝对」的逻辑改成 cwd 下的文件名。
func TestScrapeBareCommandUsesPath(t *testing.T) {
	s := Scraper{
		Argv:    []string{"sh", "-c", `echo '{"ok": true, "m3u8_url": "u", "video_name": "n"}'`},
		Dir:     t.TempDir(),
		Timeout: 10 * time.Second,
	}
	res, err := s.Scrape(context.Background(), "https://example.com", "jable")
	if err != nil {
		t.Fatalf("裸命令名应能通过 PATH 找到: %v", err)
	}
	if !res.OK {
		t.Fatalf("结果不符: %+v", res)
	}
}

func TestScrapeTimeout(t *testing.T) {
	s := Scraper{
		Argv:    []string{"/bin/sh", "-c", "sleep 5"},
		Timeout: 120 * time.Millisecond,
	}
	_, err := s.Scrape(context.Background(), "https://example.com", "jable")
	if err == nil {
		t.Fatal("应超时")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息应说明超时: %v", err)
	}
}

// 超时必须连子进程一起杀掉，否则孙进程还攥着 stdout，Wait 会一直等到它自己退出。
// 这里让 shell 在后台 sleep 30 秒：只杀父进程的话，本次调用会卡满 30 秒。
func TestScrapeTimeoutKillsProcessGroup(t *testing.T) {
	s := Scraper{
		Argv:    []string{"/bin/sh", "-c", "sleep 30 & wait"},
		Timeout: 300 * time.Millisecond,
	}
	start := time.Now()
	if _, err := s.Scrape(context.Background(), "https://example.com", "jable"); err == nil {
		t.Fatal("应超时")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("取消未杀掉进程组，耗时 %s（应接近 300ms）", elapsed)
	}
}

// 取消（用户点停止）同样要立刻返回，而不是等子进程自然结束。
func TestScrapeCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := Scraper{
		Argv:    []string{"/bin/sh", "-c", "sleep 30 & wait"},
		Timeout: 60 * time.Second,
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := s.Scrape(ctx, "https://example.com", "jable")
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("取消未及时返回，耗时 %s", elapsed)
	}
}

func TestScrapeMissingBinary(t *testing.T) {
	s := Scraper{Argv: []string{"/definitely/not/a/binary"}}
	if _, err := s.Scrape(context.Background(), "u", "jable"); err == nil {
		t.Fatal("命令不存在时应报错")
	}
}

func TestScrapeNoArgv(t *testing.T) {
	if _, err := (Scraper{}).Scrape(context.Background(), "u", "jable"); err == nil {
		t.Fatal("未配置命令时应报错")
	}
}
