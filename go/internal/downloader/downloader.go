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
	"time"

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

// childEnv 补全工具路径和无终端环境下下载器需要的 terminfo。
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

	// Unix 下 dl-re 0.6.0 即使输出到管道也会绘制进度条。TERM=dumb 的
	// terminfo 没有有效行数，.NET/Spectre 会在裁剪进度条时抛 RemoveRange 异常。
	// 仅为无终端环境补齐 TERM；正常终端配置及父进程环境保持不变。
	term := os.Getenv("TERM")
	setTerm := runtime.GOOS != "windows" && (term == "" || term == "dumb")
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") || (setTerm && strings.HasPrefix(kv, "TERM=")) {
			continue
		}
		out = append(out, kv)
	}
	if setTerm {
		out = append(out, "TERM=xterm-256color")
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
	// kill 失败（进程僵死）时给 Wait 一个兜底超时。注意它**管不到**下面的管道：
	// WaitDelay 只约束 Wait 自身，而管道两端由我们自己持有，见后面 drainReaders。
	cmd.WaitDelay = 5 * time.Second

	// 管道由自己创建，不用 cmd.StdoutPipe：前者读端归 exec 管，Wait 一返回就被关，
	// 末尾几行日志会丢；后者又只能「先读到 EOF 再 Wait」，而后代进程接着攥住写端时
	// 永远等不到 EOF（GR-02，UI 会一直显示运行中）。自有管道两头都在手上：
	// Wait 不会关它，收尾超时也能主动关掉。
	outR, outW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("无法创建下载器输出管道: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return fmt.Errorf("无法创建下载器错误管道: %w", err)
	}
	// 读端在函数返回时关掉：管道是自己建的，没有别人替我们收
	// （StdoutPipe 时代由 exec 负责）。不关就白占两个 fd，只能等 GC finalizer，
	// 那属于碰运气，连下几百条时容易撞上 fd 上限。
	defer outR.Close()
	defer errR.Close()

	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		outR.Close()
		errR.Close()
		return fmt.Errorf("启动 N_m3u8DL-RE 失败: %w", err)
	}
	// 写端已被子进程继承，父进程这份必须关掉，否则读端永远等不到 EOF。
	outW.Close()
	errW.Close()

	tail := proc.NewRing(30)

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
			// 读协程异常退出后不能继续让子进程写满管道，否则它会一直卡在写上。
			_ = proc.Kill(cmd)
		}
		done <- err
	}
	go func() {
		readOutput(outR, func(line string) {
			progressFn(line)
			if !isProgressLine(line) {
				tail.Push(line)
				emit(line)
			}
		})
	}()
	go func() {
		readOutput(errR, func(line string) {
			tail.Push(line)
			emit(line)
		})
	}()

	// 进程退出与日志读尽并行观测：Wait 必须在读之前就有人在等，
	// 否则读端被后代进程攥住时永远走不到 Wait（GR-02 的原始问题）。
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	waitErr := <-waitCh

	// 到这里进程已经退出，管道里剩下的字节应当毫秒级读完。只有后代进程还攥着
	// 写端时才会等不到 EOF，这时按上限收尾：关掉我们这一端的读口让读协程立刻
	// 返回，再把整组杀掉，免得留下孤儿进程。上限只加在「退出之后」，
	// 正常的长下载不受影响。
	readErrs, drained := drainReaders(done, 2, drainGrace, func() {
		outR.Close()
		errR.Close()
		_ = proc.Kill(cmd)
	})
	if drained {
		// 日志只收到一部分，但下载本身可能已经完成：让它进入明确终态，
		// 由上游按产物判定成败，而不是把整轮任务挂死在这里。
		emit("[warn] 日志收尾超时（可能有子进程仍持有输出管道），已按上限返回")
	}

	if ctx.Err() != nil {
		return context.Canceled
	}
	if !drained {
		// 收尾超时时读错误是我们关管道造成的，不能算下载器的问题。
		var readErr error
		for _, e := range readErrs {
			if e != nil {
				readErr = e
				break
			}
		}
		if readErr != nil {
			return fmt.Errorf("读取下载器日志失败: %w", readErr)
		}
	}
	if waitErr != nil {
		msg := tail.Summary()
		if msg != "" {
			return fmt.Errorf("下载失败: %v（%s）", waitErr, msg)
		}
		return fmt.Errorf("下载失败: %w", waitErr)
	}
	return nil
}

// drainGrace 是「进程退出/被杀」之后等待日志读尽的时长上限。
//
// 单独成变量是为了测试能调小：真实场景 5 秒足够把管道里剩下的字节读完。
var drainGrace = 5 * time.Second

// drainReaders 等 n 个读协程结束，最多等 grace；到点先 onTimeout 强制收尾，
// 再收齐剩余结果 —— 不把「日志没读完」变成「函数永远不返回」。
func drainReaders(done <-chan error, n int, grace time.Duration, onTimeout func()) ([]error, bool) {
	errs := make([]error, 0, n)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for len(errs) < n {
		select {
		case err := <-done:
			errs = append(errs, err)
		case <-timer.C:
			onTimeout()
			for len(errs) < n {
				errs = append(errs, <-done)
			}
			return errs, true
		}
	}
	return errs, false
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

// sizeUnitRe 匹配 "959.17MB" / "2.09GB" 这类体积串。
var sizeUnitRe = regexp.MustCompile(`^([\d.]+)\s*([KMGTP]?i?B)$`)

// ParseSize 把进度里的人类可读体积转成字节数。
//
// dl-re 按 1024 进制打印，这里也用 1024；调用方拿它做「成品是否明显偏小」的
// 粗略校验，阈值留得比较宽，进制差几个百分点不影响结论。
func ParseSize(s string) (int64, bool) {
	m := sizeUnitRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	// "GiB" 与 "GB" 都按 1024 进制算：这里只服务于粗判，进制差几个百分点不影响结论。
	unit := strings.ReplaceAll(strings.ToUpper(m[2]), "I", "")
	mult := float64(1)
	switch unit {
	case "KB":
		mult = 1 << 10
	case "MB":
		mult = 1 << 20
	case "GB":
		mult = 1 << 30
	case "TB":
		mult = 1 << 40
	case "PB":
		mult = 1 << 50
	}
	return int64(v * mult), true
}

// flushBytes 是「没有分隔符时先吐出去」的字节阈值。
//
// dl-re 在真实控制台下用 \r 原地重绘进度条；一旦 stdout 被重定向（本应用用的
// 就是管道），它既不打 \r 也不打 \n，整段进度帧会首尾相接，连成一条永远等不到
// 结尾的「超长行」，直到进程退出才读到。只按分隔符切分，进度就会一直停在 0%，
// 进程结束时一次性跳到 100%。所以攒够这么多字节就先增量吐一刀。
const flushBytes = 4 * 1024

// splitLines 按 \r 与 \n 切分，并在迟迟没有分隔符时按 flushBytes 增量吐出。
//
// 两个约束同时要满足：
//  1. 分隔符优先——有 \r 或 \n 就按行切，日志与进度帧都保持原样；
//  2. 不许憋着——重定向下进度帧不带任何分隔符，必须有大小阈值兜底，
//     否则整段输出要等到 EOF 才交出去，进度条就永远停在 0%。
func splitLines(r io.Reader, emit func(string)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	readBuf := make([]byte, 32*1024)
	var pending []byte
	for {
		n, readErr := br.Read(readBuf)
		if n > 0 {
			pending = append(pending, readBuf[:n]...)
			rest, err := drainLines(pending, emit)
			if err != nil {
				return err
			}
			pending = rest
		}
		if readErr == io.EOF {
			if len(pending) == 0 {
				return nil
			}
			return emitLine(pending, emit)
		}
		if readErr != nil {
			return readErr
		}
	}
}

// drainLines 从 pending 里切出能切的段并 emit，返回剩下的尾巴。
func drainLines(pending []byte, emit func(string)) ([]byte, error) {
	for {
		if i := bytes.IndexAny(pending, "\r\n"); i >= 0 {
			if i > 0 {
				if err := emitLine(pending[:i], emit); err != nil {
					return nil, err
				}
			}
			pending = pending[i+1:]
			continue
		}
		if len(pending) < flushBytes {
			return pending, nil
		}
		cut := flushCut(pending)
		if err := emitLine(pending[:cut], emit); err != nil {
			return nil, err
		}
		pending = pending[cut:]
	}
}

// flushAnchorWindow 是「切点回退到百分比」的最大回退距离。
// 真实进度帧之间约 160 字节；超过这个距离说明是一段没有进度的普通日志，
// 不必再为对齐帧结构让步。
const flushAnchorWindow = 512

// flushCut 计算增量切点：切在最后一个完整帧的末尾。
//
// 进度帧之间没有任何分隔符，切在帧中间会把同一帧的字段劈到两段里：
//   - 分片数与百分比被劈开时，前一段会解析出「上一帧的百分比 + 这一帧的分片数」
//     这种张冠李戴的结果，后一段又因为没有分片数而解析不出最后一帧（例如 100%）；
//   - 百分比之后的体积、速度被劈开时，百分比来自新帧、体积还停在上一帧。
//
// 帧的结构是「… 分片/总数 百分比 体积 速度 剩余时间」，其中剩余时间与速度都可能
// 缺席，所以以最后一个百分比为锚，连上它后面第一个收尾字段（剩余时间，没有就是
// 速度）作为切点。切在帧末，被跳过的帧整帧留在 pending 里等下一次读出，两段都不
// 会出现跨帧字段。
func flushCut(pending []byte) int {
	limit := flushBytes
	if limit > len(pending) {
		limit = len(pending)
	}
	window := pending[:limit]

	// 剩余时间、速度、百分比都是 ASCII，紧跟其后切一定落在字符边界上。
	i := bytes.LastIndexByte(window, '%')
	if i < 0 || limit-i > flushAnchorWindow {
		return safeCut(pending, limit)
	}
	// 从最后一个百分比往前试：窗口刚好截断在帧中间时，本帧的收尾字段还没读到，
	// 退到上一帧再看，绝不切在半帧上。
	for ; i >= 0; i = bytes.LastIndexByte(window[:i], '%') {
		rest := window[i+1:]
		if e := etaRe.FindIndex(rest); e != nil {
			return i + 1 + e[1]
		}
		if s := speedRe.FindIndex(rest); s != nil {
			end := i + 1 + s[1]
			// 速度后面通常还跟着剩余时间（至少 8 字节）。贴着窗口末尾说明还没读全，
			// 这时不能把速度当帧尾，退一帧再切。
			if limit-end >= minETALen {
				return end
			}
		}
	}
	return safeCut(pending, limit)
}

// minETALen 是「hh:mm:ss」的最小长度，用于判断速度后面是否还有剩余时间没读全。
const minETALen = 8

// emitLine 解码一段原始字节并交出去。
func emitLine(raw []byte, emit func(string)) error {
	line, err := decodeOutput(raw)
	if err != nil {
		return err
	}
	emit(line)
	return nil
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
