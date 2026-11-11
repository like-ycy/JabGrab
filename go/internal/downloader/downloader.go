// Package downloader 封装对 N_m3u8DL-RE 的调用与进度解析。
package downloader

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

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

// findFFmpeg 是 FindFFmpeg 的间接引用，测试里可替换成固定返回值。
var findFFmpeg = FindFFmpeg

// FindFFmpeg 查找 ffmpeg，找不到返回空串。
//
// 查找顺序：PATH → near 指定的目录（如下载器所在目录，便携部署常把两者放一起）
// → 系统常见目录。
func FindFFmpeg(near ...string) string {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	dirs := make([]string, 0, len(near)+len(extraBinDirs))
	dirs = append(dirs, near...)
	dirs = append(dirs, extraBinDirs...)
	for _, d := range dirs {
		if d == "" || d == "." {
			continue
		}
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

	// ffmpeg 是 dl-re 的启动期硬依赖（缺了直接抛异常退出），与是否混流无关，
	// 所以只要拿到路径就显式传入，避免依赖子进程自己去 PATH 里找。
	if o.FFmpegPath != "" {
		args = append(args, "--ffmpeg-binary-path", o.FFmpegPath)
	}
	if o.MuxToMP4 {
		args = append(args, "--mux-after-done", "format=mp4")
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

	// 下面会把工作目录切到保存目录，届时相对路径的可执行文件会按新目录解析，
	// 因此先把带路径分隔符的相对路径转成绝对路径（裸命令名仍交给 PATH 查找）。
	// Windows 上 "bin/x.exe" 这种正斜杠写法也合法，两种分隔符都要认。
	bin := o.Bin
	if !filepath.IsAbs(bin) && strings.ContainsAny(bin, `/\`) {
		if abs, err := filepath.Abs(bin); err == nil {
			bin = abs
		}
	}

	// 保存目录同时用作工作目录和 --save-dir，必须先绝对化：
	// 相对路径会被二次拼接成 out/out，流水线的「已存在」检查也会看错地方。
	if o.SaveDir != "" {
		if abs, err := filepath.Abs(o.SaveDir); err == nil {
			o.SaveDir = abs
		}
		if err := os.MkdirAll(o.SaveDir, 0o755); err != nil {
			return fmt.Errorf("无法创建保存目录 %s: %w", o.SaveDir, err)
		}
	}

	// ffmpeg 是启动期硬依赖：缺失时 dl-re 会抛出未捕获异常直接退出，
	// 不论是否要求混流。所以先解析到可用路径，拿不到就不启动。
	// 除了 PATH 和系统目录，还要看下载器所在目录——便携部署常把两者放在一起。
	if o.FFmpegPath == "" {
		o.FFmpegPath = findFFmpeg(filepath.Dir(bin))
	}
	if o.FFmpegPath == "" {
		return fmt.Errorf("未找到 ffmpeg：N_m3u8DL-RE 启动时强制校验 ffmpeg，" +
			"请先安装（macOS 可用 brew install ffmpeg，Windows 把 ffmpeg.exe 所在目录加入 PATH，" +
			"或把 ffmpeg 放在下载器同目录）后重试")
	}

	emit := func(string) {}
	if onLog != nil {
		emit = onLog
	}

	cmd := exec.CommandContext(ctx, bin, o.Args(m3u8)...) //nolint:gosec // 路径与参数均来自本地配置
	cmd.Dir = o.SaveDir
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

	done := make(chan error, 2)
	readOutput := func(r io.Reader, emit func(string)) {
		err := splitLines(r, emit)
		if err != nil {
			// 读协程退出后不能继续让子进程写满管道，否则 Wait 会一直阻塞。
			_ = proc.Kill(cmd)
		}
		done <- err
	}
	go func() {
		readOutput(stdout, func(line string) {
			progressFn(line)
			if !isProgressLine(line) {
				tail.push(line)
				emit(line)
			}
		})
	}()
	go func() {
		readOutput(stderr, func(line string) {
			tail.push(line)
			emit(line)
		})
	}()

	readErr, otherReadErr := <-done, <-done
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return context.Canceled
	}
	if readErr == nil {
		readErr = otherReadErr
	}
	if readErr != nil {
		return fmt.Errorf("读取下载器日志失败: %w", readErr)
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
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		line, err := decodeOutput(sc.Bytes())
		if err != nil {
			return err
		}
		emit(line)
	}
	return sc.Err()
}

// ring 保存最后 N 行，用于错误摘要。
//
// stdout 与 stderr 各由一个协程写入，必须加锁，否则并发 append/切片会丢行甚至损坏数据。
type ring struct {
	mu    sync.Mutex
	limit int
	buf   []string
}

func (r *ring) push(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, s)
	if len(r.buf) > r.limit {
		r.buf = r.buf[1:]
	}
}

func (r *ring) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// ActorDir 把演员列表压成一级目录名，多个演员之间用 "-" 连接。
//
// 用横线而不是逗号：逗号在部分平台/工具链上是保留或需要转义的字符，
// 横线在 Windows 与 macOS 上都不受限。名字本身的非法字符一并交给 SafeName。
// 返回空串表示没有可用演员名，调用方应直接放在保存目录根下。
func ActorDir(actors []string) string {
	names := make([]string, 0, len(actors))
	for _, a := range actors {
		if name := SafeName(a); name != "" {
			names = append(names, name)
		}
	}
	// Windows 不允许目录名以点或空格结尾
	joined := strings.Trim(strings.Join(names, "-"), " .")
	return clampDirName(joined)
}

// actorDirMaxRunes 是演员目录名的字符上限。
//
// 文件系统单段名上限是 255 字节，而日文姓名 UTF-8 下 3 字节/字，
// 大合辑类的作品动辄十几个演员，直接拼会超限——MkdirAll 报
// "file name too long"，本来能下的片子会整个失败。
const actorDirMaxRunes = 60

// clampDirName 截断过长的目录名，并补一段短哈希保持区分度。
//
// 只截断不加哈希的话，不同演员组合可能落到同一个目录里；
// 加上原名的哈希后，同一份演员名单每次算出的目录名仍然固定。
func clampDirName(name string) string {
	runes := []rune(name)
	if len(runes) <= actorDirMaxRunes {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name)) // fnv 的 Write 不会失败
	return fmt.Sprintf("%s-%08x", string(runes[:actorDirMaxRunes]), h.Sum32())
}
