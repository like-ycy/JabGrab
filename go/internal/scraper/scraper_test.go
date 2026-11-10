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
