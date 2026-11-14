package downloader

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// stdout 被重定向后，dl-re 的进度帧不带任何分隔符（实测 \r 与 \n 都是 0 个），
// 整段进度会连成一条等不到结尾的超长行。必须按字节阈值增量吐出，
// 否则整段输出要等到 EOF 才交出去，界面进度条就会一直停在 0%（本次回归点）。
func TestSplitLinesFlushesProgressWithoutDelimiter(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()

	var (
		mu   sync.Mutex
		got  []string
		live = make(chan string, 128)
	)
	done := make(chan error, 1)
	go func() {
		done <- splitLines(pr, func(s string) {
			mu.Lock()
			got = append(got, s)
			mu.Unlock()
			select {
			case live <- s:
			default:
			}
		})
	}()

	frame := func(pct int) string {
		return fmt.Sprintf("Vid 1920x1080 | 6221 Kbps %s %d/100 %d.00%% 68.64MB/732.18MB 3.60MBps 00:03:31",
			strings.Repeat("━", 30), pct, pct)
	}
	frames := flushBytes/len(frame(1)) + 8

	written := make(chan struct{})
	go func() {
		defer close(written)
		for i := 1; i <= frames; i++ {
			if _, err := pw.Write([]byte(frame(i))); err != nil {
				return
			}
		}
	}()

	// 攒够 flushBytes 就应该先吐出一段，而不是憋到 close。
	select {
	case line := <-live:
		if _, ok := ParseProgress(line); !ok {
			t.Fatalf("增量吐出的片段应能解析出进度: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有分隔符时必须增量吐出进度帧，实际一直等到 EOF（进度会停在 0%）")
	}

	<-written
	if err := pw.Close(); err != nil {
		t.Fatalf("关闭管道失败: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("splitLines 出错: %v", err)
	}

	mu.Lock()
	lines := append([]string(nil), got...)
	mu.Unlock()
	if len(lines) < 2 {
		t.Fatalf("整段进度应被切成多段增量输出，实际只有 %d 段", len(lines))
	}
	last, ok := ParseProgress(lines[len(lines)-1])
	if !ok || last.Percent != float64(frames) {
		t.Fatalf("最后一段应带上最新一帧进度 %d%%，实际: %q", frames, lines[len(lines)-1])
	}
}

// 增量切点必须落在字符边界，否则长段中文会被劈成半个字、解码成乱码。
//
// 只能在非 Windows 上断言「拼回来等于原文」：Windows 的 decodeOutput 会按系统
// ACP 解码，UTF-8 输入在这一层本来就不成立（中文 Windows 上是 936），切点边界
// 由 TestSafeCutOnMultibyte 单独覆盖。
func TestSplitLinesFlushKeepsMultibyteIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("decodeOutput 按系统代码页解码，这里只验证 UTF-8 平台上的切点安全性")
	}
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("演员姓名甲乙丙")
	}
	in := sb.String()

	var out strings.Builder
	if err := splitLines(strings.NewReader(in), func(s string) { out.WriteString(s) }); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Fatal("拼接结果与原文不一致：切点切坏了多字节字符")
	}
	if strings.ContainsRune(out.String(), utf8.RuneError) {
		t.Fatal("出现替换字符，说明切点落在多字节字符中间")
	}
}

// safeCut 的契约：任意 limit 都不能切在多字节字符中间，且不能超过 limit。
//
// 两个平台的判据不同，验证方式也不同：UTF-8 看前缀是否合法，DBCS 用「分段解码
// 再拼起来必须等于整段解码」来验——只有没劈坏字符时才成立。
func TestSafeCutOnMultibyte(t *testing.T) {
	var sample []byte
	if runtime.GOOS == "windows" {
		// 直接给 GBK 字节：中文 Windows 上是双字节字符（真正的考验），
		// 其它代码页上这些都是单字节，随便怎么切都成立，同样不会误报。
		sample = []byte("abc\xd6\xd0\xce\xc4 def\xd6\xd0")
	} else {
		sample = []byte("abc演员姓名甲乙丙 def演员")
	}

	whole, err := decodeOutput(sample)
	if err != nil {
		t.Fatalf("解码样本失败: %v", err)
	}
	for limit := 0; limit <= len(sample); limit++ {
		cut := safeCut(sample, limit)
		if cut < 0 || cut > limit {
			t.Fatalf("limit=%d 切点越界: %d", limit, cut)
		}
		left, err1 := decodeOutput(sample[:cut])
		right, err2 := decodeOutput(sample[cut:])
		if err1 != nil || err2 != nil {
			t.Fatalf("limit=%d 分段解码失败: %v %v", limit, err1, err2)
		}
		if left+right != whole {
			t.Fatalf("limit=%d 切点 %d 劈开了字符：%q + %q != %q", limit, cut, left, right, whole)
		}
		if runtime.GOOS != "windows" && !utf8.Valid(sample[:cut]) {
			t.Fatalf("limit=%d 切点 %d 把 UTF-8 前缀切坏了", limit, cut)
		}
	}

	if runtime.GOOS != "windows" {
		// 读取边界可能正好停在多字节字符中间：末尾那半个字必须留到下一次，
		// 不能因为「已经在缓冲区末尾」就放行。
		partial := []byte("abcd" + "演"[:2])
		if cut := safeCut(partial, len(partial)); cut != len("abcd") {
			t.Fatalf("末尾半字符应被留下：切点=%d，期望 4", cut)
		}
	}
}

// 进度帧之间没有分隔符，flush 切点若落在帧中间，会把同一帧的分片数与百分比
// 劈到两段里：前一段出现「上一帧的百分比 + 这一帧的分片数」的张冠李戴，后一段
// 又因为没有分片数而解析不出来，最后那帧 100% 就彻底丢了。体积同理，必须和
// 百分比来自同一帧。这里让体积与分片数一一对应，穷举切点对齐方式逐段核对。
//
// 两种真实帧格式都要覆盖：带剩余时间的（Vid）和只以速度收尾的（Aud/Sub）。
func TestSplitLinesFlushKeepsFinalProgress(t *testing.T) {
	total := 47
	formats := map[string]func(pad, pct, cur int) string{
		"with-eta": func(pad, pct, cur int) string {
			return fmt.Sprintf("Vid 1920x1080 | 6221 Kbps %s%s %d/%d %d.00%% %d.00MB/%d.00MB 3.60MBps 00:03:31",
				strings.Repeat("━", 30), strings.Repeat(" ", pad), cur, total, pct, cur, total)
		},
		"no-eta": func(pad, pct, cur int) string {
			return fmt.Sprintf("Aud Kbps -- %s%s %d/%d %d.00%% %d.00MB/%d.00MB %d.00KBps",
				strings.Repeat("-", 30), strings.Repeat(" ", pad), cur, total, pct, cur, total, cur)
		},
	}
	for name, mk := range formats {
		t.Run(name, func(t *testing.T) {
			for pad := 0; pad < 300; pad++ {
				var sb strings.Builder
				for i := 1; i < total; i++ {
					sb.WriteString(mk(pad, i*100/total, i))
				}
				sb.WriteString(mk(pad, 100, total))

				var last string
				if err := splitLines(strings.NewReader(sb.String()), func(s string) {
					// 每一段里，体积必须和分片数来自同一帧
					if p, ok := ParseProgress(s); ok {
						if want := fmt.Sprintf("%d.00MB", p.Current); p.Downloaded != want {
							t.Fatalf("pad=%d 体积与百分比错配: 分片=%d 体积=%q 期望=%q 段尾=%q",
								pad, p.Current, p.Downloaded, want, tailOf(s, 80))
						}
					}
					last = s
				}); err != nil {
					t.Fatal(err)
				}
				p, ok := ParseProgress(last)
				if !ok || p.Percent != 100 || p.Current != total || p.Total != total {
					t.Fatalf("pad=%d 最后一帧被切坏了: 解析=%+v ok=%v 末段=%q", pad, p, ok, tailOf(last, 80))
				}
			}
		})
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
