package downloader

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
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

// ffmpeg 是 dl-re 的启动期硬依赖，即便不混流也必须显式传入路径。
func TestArgsPassesFFmpegWithoutMux(t *testing.T) {
	o := Options{SaveDir: "/tmp/o", SaveName: "n", MuxToMP4: false, FFmpegPath: "/opt/homebrew/bin/ffmpeg"}
	args := o.Args("u")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ffmpeg-binary-path /opt/homebrew/bin/ffmpeg") {
		t.Fatalf("不混流也应传 ffmpeg 路径: %v", args)
	}
	if strings.Contains(joined, "--mux-after-done") {
		t.Fatalf("未开混流不该出现 --mux-after-done: %v", args)
	}
}

// 缺 ffmpeg 时要在启动子进程之前就报清晰的错，
// 而不是让它抛未捕获异常、只留一行 stderr。
func TestRunFailsWithoutFFmpeg(t *testing.T) {
	old := findFFmpeg
	findFFmpeg = func(...string) string { return "" }
	defer func() { findFFmpeg = old }()

	o := Options{Bin: "/bin/echo", SaveDir: t.TempDir(), SaveName: "n"}
	err := o.Run(context.Background(), "u", nil, nil)
	if err == nil {
		t.Fatal("缺 ffmpeg 应报错")
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("错误信息应点明 ffmpeg: %v", err)
	}
}

// stdout 与 stderr 各一个协程写同一个 ring，必须在 -race 下无告警。
func TestRingConcurrentPush(t *testing.T) {
	r := &ring{limit: 8}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.push("line")
			}
		}()
	}
	wg.Wait()
	if n := len(r.buf); n != 8 {
		t.Fatalf("ring 应保持 limit 条，实际 %d", n)
	}
	if r.summary() == "" {
		t.Fatal("摘要不该为空")
	}
}

// 下载器必须在保存目录里运行：Finder 启动的 .app 的 cwd 是 /（不可写），
// 只给 --save-dir 不够，临时分片仍会被写到不可写的位置。
func TestRunUsesSaveDirAsWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX shell")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-dlre.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	saveDir := filepath.Join(dir, "out")

	var got []string
	// ffmpeg 给个非空占位即可：假下载器不看这个参数，测试也就不依赖开发机是否装了 ffmpeg。
	o := Options{Bin: script, SaveDir: saveDir, SaveName: "n", FFmpegPath: "/usr/bin/false"}
	if err := o.Run(context.Background(), "u", nil, func(l string) {
		got = append(got, l)
	}); err != nil {
		t.Fatalf("应正常退出: %v", err)
	}

	wantDir, err := filepath.EvalSymlinks(saveDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range got {
		if p, err := filepath.EvalSymlinks(strings.TrimSpace(l)); err == nil && p == wantDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("子进程工作目录应是保存目录 %s，实际输出: %v", wantDir, got)
	}
}

// 配置里用相对路径指定下载器时，切换工作目录后仍要能找到它。
// （旧实现把 Dir 设成保存目录后，相对路径会按新目录二次解析而失效。）
func TestRunResolvesRelativeBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX shell")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-dlre.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho started\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	o := Options{Bin: "./fake-dlre.sh", SaveDir: filepath.Join(dir, "out"), SaveName: "n", FFmpegPath: "/usr/bin/false"}
	if err := o.Run(context.Background(), "u", nil, nil); err != nil {
		t.Fatalf("相对路径的下载器也应能启动: %v", err)
	}
}

// 保存目录写成相对路径时，必须绝对化后再同时用作工作目录与 --save-dir，
// 否则会拼成 out/out，流水线的「已存在」检查也会指向错误位置。
func TestRunAbsolutizesRelativeSaveDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX shell")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-dlre.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	var got []string
	o := Options{Bin: script, SaveDir: "out", SaveName: "n", FFmpegPath: "/usr/bin/false"}
	if err := o.Run(context.Background(), "u", nil, func(l string) {
		got = append(got, l)
	}); err != nil {
		t.Fatalf("应成功: %v", err)
	}

	wantDir, err := filepath.EvalSymlinks(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range got {
		if p, err := filepath.EvalSymlinks(strings.TrimSpace(l)); err == nil && p == wantDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("工作目录应为绝对路径 %s，实际输出: %v", wantDir, got)
	}
	// 关键回归点：不允许出现 out/out
	if st, err := os.Stat(filepath.Join(wantDir, "out")); err == nil && st.IsDir() {
		t.Fatal("保存目录被二次拼接成了 out/out")
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

// 多个演员必须用横线连接：逗号在部分平台/工具链上并不总是安全字符。
func TestActorDir(t *testing.T) {
	cases := []struct {
		actors []string
		want   string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"  "}, ""},
		{[]string{"釈アリス"}, "釈アリス"},
		{[]string{"釈アリス", "三上悠亜"}, "釈アリス-三上悠亜"},
		{[]string{"釈アリス", "", "三上悠亜"}, "釈アリス-三上悠亜"},
		// 名字里的路径分隔符要先清理，不能变成子目录层级
		{[]string{"a/b"}, "a_b"},
		// Windows 不允许目录名以点或空格结尾
		{[]string{"釈アリス... "}, "釈アリス"},
	}
	for _, c := range cases {
		if got := ActorDir(c.actors); got != c.want {
			t.Fatalf("ActorDir(%q)=%q want=%q", c.actors, got, c.want)
		}
	}
}

// 大合辑动辄十几个演员，拼出来的目录名会超过文件系统单段 255 字节上限：
// 必须截断（且保持同名单稳定、不同名单不撞）。
func TestActorDirClampsLongNames(t *testing.T) {
	long := make([]string, 50)
	for i := range long {
		long[i] = "演员姓名甲乙丙"
	}
	got := ActorDir(long)
	if n := len([]rune(got)); n > actorDirMaxRunes+9 {
		t.Fatalf("目录名仍然过长（%d 字符）: %q", n, got)
	}
	if n := len(got); n > 255 {
		t.Fatalf("目录名超过 255 字节: %d", n)
	}
	if again := ActorDir(long); again != got {
		t.Fatalf("同一份名单应稳定: %q != %q", got, again)
	}

	// 截断前缀相同的不同名单，必须靠哈希区分开
	other := append([]string{}, long...)
	other[len(other)-1] = "演员姓名甲乙丁"
	if ActorDir(other) == got {
		t.Fatalf("不同名单不应落到同一目录: %q", got)
	}
}
