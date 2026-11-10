package downloader

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// 来自真实 Windows 版 dl-re 的输出
const realProgressLine = "Vid Kbps ------------------------------ 1200/2675 44.86% 959.17MB/2.09GB 6.11MBps 00:04:12"

func TestParseProgress(t *testing.T) {
	p, ok := ParseProgress(realProgressLine)
	if !ok {
		t.Fatalf("应能解析真实进度行: %q", realProgressLine)
	}
	want := Progress{
		Current:    1200,
		Total:      2675,
		Percent:    44.86,
		Downloaded: "959.17MB",
		TotalSize:  "2.09GB",
		Speed:      "6.11MBps",
		ETA:        "00:04:12",
	}
	if p != want {
		t.Fatalf("解析结果不符\n got=%+v\nwant=%+v", p, want)
	}
}

func TestParseProgressVariants(t *testing.T) {
	cases := []struct {
		line string
		want Progress
	}{
		{
			line: "\x1b[32mVid Kbps -- 10/50 20.00% 1.00MB/5.00MB 512KBps 00:00:05\x1b[0m",
			want: Progress{Current: 10, Total: 50, Percent: 20, Downloaded: "1.00MB",
				TotalSize: "5.00MB", Speed: "512KBps", ETA: "00:00:05"},
		},
		{
			// 没有剩余时间的行
			line: "Aud Kbps -- 3/9 33.33% 300KB/900KB 100KBps",
			want: Progress{Current: 3, Total: 9, Percent: 33.33, Downloaded: "300KB",
				TotalSize: "900KB", Speed: "100KBps"},
		},
	}

	for _, c := range cases {
		got, ok := ParseProgress(c.line)
		if !ok {
			t.Fatalf("解析失败: %q", c.line)
		}
		if got != c.want {
			t.Fatalf("解析不符 %q\n got=%+v\nwant=%+v", c.line, got, c.want)
		}
	}
}

func TestParseProgressIgnoresNonProgress(t *testing.T) {
	for _, line := range []string{
		"02:04:36.747 INFO : Vid *AES_128 Kbps | 2675 Segments | ~02h58m19s",
		"02:04:36.757 INFO : 保存文件名: CJOD-548-C",
		"",
		"",
	} {
		if _, ok := ParseProgress(line); ok {
			t.Fatalf("不该把普通日志识别成进度: %q", line)
		}
	}
}

// macOS 实机抓取的真实进度行：字段可能没有空格分隔（光标重绘叠加）。
func TestParseProgressGlued(t *testing.T) {
	cases := []struct {
		line string
		want Progress
	}{
		{
			"Vid 1920x1080 | 6221 Kbps ━━━━━━━━━━━━━━━ 6/64 9.38% 68.64MB/732.18MB 3.60MBps 00:03:31",
			Progress{Current: 6, Total: 64, Percent: 9.38, Downloaded: "68.64MB",
				TotalSize: "732.18MB", Speed: "3.60MBps", ETA: "00:03:31"},
		},
		{
			// 上一帧的尾巴和下一条进度粘连在一起
			"48.62MB/1.01GB3.59MBps00:05:40Vid 1920x1080 | 6221 Kbps ━━━ 3/64 4.69% 48.62MB/1.01GB 3.59MBps 00:05:40",
			Progress{Current: 3, Total: 64, Percent: 4.69, Downloaded: "48.62MB",
				TotalSize: "1.01GB", Speed: "3.59MBps", ETA: "00:05:40"},
		},
		{
			"1/64 1.56% 5.39MB/345.17MB2.09MBps00:02:44Vid 1920x1080",
			Progress{Current: 1, Total: 64, Percent: 1.56, Downloaded: "5.39MB",
				TotalSize: "345.17MB", Speed: "2.09MBps", ETA: "00:02:44"},
		},
	}
	for _, c := range cases {
		got, ok := ParseProgress(c.line)
		if !ok {
			t.Fatalf("解析失败: %q", c.line)
		}
		if got != c.want {
			t.Fatalf("解析不符 %q\n got=%+v\nwant=%+v", c.line, got, c.want)
		}
	}
}

func TestArgs(t *testing.T) {
	o := Options{
		SaveDir:    "/tmp/out",
		SaveName:   "ABC-123",
		Threads:    25,
		RetryCount: 20,
		TimeoutSec: 30,
		MaxSpeed:   "15M",
		MuxToMP4:   true,
		Headers:    map[string]string{"user-agent": "UA", "referer": "https://x"},
	}
	args := o.Args("https://cdn/x.m3u8")

	want := []string{
		"https://cdn/x.m3u8",
		"--save-dir", "/tmp/out",
		"--save-name", "ABC-123",
		"--thread-count", "25",
		"--download-retry-count", "20",
		"--http-request-timeout", "30",
		"--max-speed", "15M",
		"--no-ansi-color", "--auto-select",
		"--mux-after-done", "format=mp4",
		"-H", "referer: https://x",
		"-H", "user-agent: UA",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("参数不符\n got=%v\nwant=%v", args, want)
	}
}

func TestArgsMinimal(t *testing.T) {
	o := Options{SaveDir: "/tmp/out", SaveName: "n"}
	args := o.Args("u")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"--max-speed", "--mux-after-done", "-H"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("未配置时不应出现 %s: %v", forbidden, args)
		}
	}
}

// dl-re 重绘进度条用 \r，必须按 \r 与 \n 同时切分。
func TestSplitLinesHandlesCarriageReturn(t *testing.T) {
	in := "a\rb\r\nc\nd\r"
	var got []string
	if err := splitLines(strings.NewReader(in), func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("切分结果不符 got=%v want=%v", got, want)
	}
}

func TestArgsWithFFmpegPath(t *testing.T) {
	o := Options{SaveDir: "/tmp/o", SaveName: "n", MuxToMP4: true, FFmpegPath: "/opt/homebrew/bin/ffmpeg"}
	args := o.Args("u")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ffmpeg-binary-path /opt/homebrew/bin/ffmpeg") {
		t.Fatalf("应显式传入 ffmpeg 路径: %v", args)
	}
}

// 从 Finder 启动的 .app 只有极简 PATH，必须补上 Homebrew 目录。
func TestChildEnvAugmentsPath(t *testing.T) {
	var path string
	for _, kv := range childEnv() {
		if strings.HasPrefix(kv, "PATH=") {
			path = kv
		}
	}
	if path == "" {
		t.Fatal("childEnv 应包含 PATH")
	}
	if runtime.GOOS == "darwin" && !strings.Contains(path, "/opt/homebrew/bin") {
		t.Fatalf("PATH 未补全 Homebrew 目录: %s", path)
	}
	if strings.Count(path, "PATH=") != 1 {
		t.Fatalf("PATH 重复: %s", path)
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"CJOD-548-C":      "CJOD-548-C",
		"a/b:c*d?e":       "a_b_c_d_e",
		"  前后空格  ":        "前后空格",
		"换\n行":            "换 行",
		"normal title 01": "normal title 01",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Fatalf("SafeName(%q)=%q want=%q", in, got, want)
		}
	}
}
