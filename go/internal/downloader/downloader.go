// Package downloader 封装对 N_m3u8DL-RE 的调用与进度解析。
package downloader

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"jabgrab/internal/proc"
)

// Progress 是进度条一行解析出的结果。
//
// 对应 N_m3u8DL-RE 的输出：
//
//	Vid Kbps ------ 1200/2675 44.86% 959.17MB/2.09GB 6.11MBps 00:04:12
type Progress struct {
	Current    int     `json:"current"`    // 已完成分片
	Total      int     `json:"total"`      // 总分片
	Percent    float64 `json:"percent"`    // 0-100
	Downloaded string  `json:"downloaded"` // 959.17MB
	TotalSize  string  `json:"totalSize"`  // 2.09GB
	Speed      string  `json:"speed"`      // 6.11MBps
	ETA        string  `json:"eta"`        // 00:04:12
}

// Options 是一次下载的全部参数，来自配置。
type Options struct {
	Bin        string
	SaveDir    string
	SaveName   string
	Threads    int
	RetryCount int
	TimeoutSec int
	MaxSpeed   string
	MuxToMP4   bool
	FFmpegPath string // 显式指定 ffmpeg，空则由 dl-re 自行查找
	Headers    map[string]string
}

// GUI 应用从 Finder 启动时 PATH 往往只有 /usr/bin:/bin，
// 找不到 Homebrew 装的 ffmpeg，这里补上常见目录。
var extraBinDirs = []string{
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"/opt/local/bin",
	"/usr/bin",
	"/bin",
}

// FindFFmpeg 在 PATH 与常见目录里查找 ffmpeg，找不到返回空串。
func FindFFmpeg() string {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range extraBinDirs {
		cand := filepath.Join(d, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// childEnv 返回补全了常见 bin 目录的 PATH。
func childEnv() []string {
	env := os.Environ()
	current := os.Getenv("PATH")
	parts := strings.Split(current, string(os.PathListSeparator))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		seen[p] = true
	}
	for _, d := range extraBinDirs {
		if !seen[d] {
			parts = append(parts, d)
			seen[d] = true
		}
	}
	merged := strings.Join(parts, string(os.PathListSeparator))

	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+merged)
}

var (
	// 进度行的字段并不总是被空格分开：dl-re 用光标上移重绘进度条，
	// 去掉 ANSI 控制符后会出现 "48.62MB/1.01GB3.59MBps00:05:40" 这种粘连，
	// 因此按「单位」锚定逐个字段提取，而不是按空白切分。
	segRe   = regexp.MustCompile(`(\d+)/(\d+)`)
	pctRe   = regexp.MustCompile(`([\d.]+)%`)
	sizeRe  = regexp.MustCompile(`([\d.]+\s*[KMGTP]?i?B)\s*/\s*([\d.]+\s*[KMGTP]?i?B)`)
	speedRe = regexp.MustCompile(`([\d.]+\s*[KMGTP]?i?B(?:ps|/s))`)
	etaRe   = regexp.MustCompile(`(\d{1,3}:\d{2}:\d{2})`)
	ansiRe  = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// lastMatch 返回最后一个匹配的捕获组。
// 粘连行里可能残留上一帧的数据，取最后一段才是当前帧。
func lastMatch(re *regexp.Regexp, s string) []string {
	all := re.FindAllStringSubmatch(s, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// Args 拼装 N_m3u8DL-RE 命令行参数（不含可执行文件本身）。
func (o Options) Args(m3u8 string) []string {
	args := []string{m3u8}

	args = append(args, "--save-dir", o.SaveDir)
	if o.SaveName != "" {
		args = append(args, "--save-name", o.SaveName)
	}
	if o.Threads > 0 {
		args = append(args, "--thread-count", strconv.Itoa(o.Threads))
	}
	if o.RetryCount > 0 {
		args = append(args, "--download-retry-count", strconv.Itoa(o.RetryCount))
	}
	if o.TimeoutSec > 0 {
		args = append(args, "--http-request-timeout", strconv.Itoa(o.TimeoutSec))
	}
	if s := strings.TrimSpace(o.MaxSpeed); s != "" {
		args = append(args, "--max-speed", s)
	}

	// 去掉 ANSI 控制符，否则捕获到的 stdout 充满转义序列，无法解析进度。
	args = append(args, "--no-ansi-color", "--auto-select")
	if o.MuxToMP4 {
		args = append(args, "--mux-after-done", "format=mp4")
		if o.FFmpegPath != "" {
			args = append(args, "--ffmpeg-binary-path", o.FFmpegPath)
		}
	}

	// headers 顺序固定，便于日志与测试复现
	keys := make([]string, 0, len(o.Headers))
	for k := range o.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := o.Headers[k]; v != "" {
			args = append(args, "-H", k+": "+v)
		}
	}
	return args
}

// Run 执行下载。onProgress 在解析到进度时回调（百分比变化才触发），
// onLog 收到下载器的全部输出行。ctx 取消会杀掉整个进程组。
func (o Options) Run(
	ctx context.Context, m3u8 string,
	onProgress func(Progress), onLog func(string),
) error {
	if o.Bin == "" {
		return fmt.Errorf("未指定 N_m3u8DL-RE 路径")
	}
	emit := func(string) {}
	if onLog != nil {
		emit = onLog
	}

	cmd := exec.CommandContext(ctx, o.Bin, o.Args(m3u8)...) //nolint:gosec // 路径与参数均来自本地配置
	cmd.Env = childEnv()
	proc.Prepare(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	cmd.WaitDelay = 0

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("无法读取下载器输出: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("无法读取下载器错误输出: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 N_m3u8DL-RE 失败: %w", err)
	}

	tail := &ring{limit: 30}

	var lastPct = -1.0
	progressFn := func(line string) {
		p, ok := ParseProgress(line)
		if !ok {
			return
		}
		if p.Percent == lastPct {
			return
		}
		lastPct = p.Percent
		if onProgress != nil {
			onProgress(p)
		}
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		_ = splitLines(stdout, func(line string) {
			progressFn(line)
			if !isProgressLine(line) {
				tail.push(line)
				emit(line)
			}
		})
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		_ = splitLines(stderr, func(line string) {
			tail.push(line)
			emit(line)
		})
	}()

	<-done
	<-done
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return context.Canceled
	}
	if waitErr != nil {
		msg := tail.summary()
		if msg != "" {
			return fmt.Errorf("下载失败: %v（%s）", waitErr, msg)
		}
		return fmt.Errorf("下载失败: %w", waitErr)
	}
	return nil
}

// ParseProgress 解析一行进度输出。
//
// 兼容两种真实格式：
//
//	Vid Kbps ------ 1200/2675 44.86% 959.17MB/2.09GB 6.11MBps 00:04:12   (Windows)
//	Vid 1920x1080 | 6221 Kbps ━━━ 6/64 9.38% 68.64MB/732.18MB 3.60MBps 00:03:31
func ParseProgress(line string) (Progress, bool) {
	clean := ansiRe.ReplaceAllString(line, "")
	if !strings.Contains(clean, "%") {
		// 没有百分比的一定不是进度行，先挡住绝大多数普通日志
		return Progress{}, false
	}

	seg := lastMatch(segRe, clean)
	pct := lastMatch(pctRe, clean)
	if seg == nil || pct == nil {
		return Progress{}, false
	}
	current, err1 := strconv.Atoi(seg[1])
	total, err2 := strconv.Atoi(seg[2])
	percent, err3 := strconv.ParseFloat(pct[1], 64)
	if err1 != nil || err2 != nil || err3 != nil || total <= 0 {
		return Progress{}, false
	}

	p := Progress{Current: current, Total: total, Percent: percent}
	if size := lastMatch(sizeRe, clean); size != nil {
		p.Downloaded, p.TotalSize = size[1], size[2]
	}
	if spd := lastMatch(speedRe, clean); spd != nil {
		p.Speed = spd[1]
	}
	if eta := lastMatch(etaRe, clean); eta != nil {
		p.ETA = eta[1]
	}
	return p, true
}

func isProgressLine(line string) bool {
	_, ok := ParseProgress(line)
	return ok
}

// splitLines 按 \r 与 \n 切分。dl-re 重绘进度条用的是回车符，
// 只按 \n 切会把整段进度糊成一行。
func splitLines(r io.Reader, emit func(string)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var sb strings.Builder
	for {
		ch, _, err := br.ReadRune()
		if err != nil {
			if sb.Len() > 0 {
				emit(sb.String())
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		if ch == '\r' || ch == '\n' {
			if sb.Len() > 0 {
				emit(sb.String())
				sb.Reset()
			}
			continue
		}
		sb.WriteRune(ch)
		if sb.Len() >= 8192 {
			emit(sb.String())
			sb.Reset()
		}
	}
}

type ring struct {
	limit int
	buf   []string
}

func (r *ring) push(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	r.buf = append(r.buf, s)
	if len(r.buf) > r.limit {
		r.buf = r.buf[1:]
	}
}

func (r *ring) summary() string {
	if len(r.buf) == 0 {
		return ""
	}
	line := strings.TrimSpace(r.buf[len(r.buf)-1])
	if len(line) > 240 {
		line = line[len(line)-240:]
	}
	return line
}

// SafeName 清理文件名中不安全的字符，避免标题里的斜杠等破坏保存路径。
func SafeName(s string) string {
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
		"\n", " ", "\r", " ", "\t", " ",
	)
	return strings.TrimSpace(replacer.Replace(s))
}
