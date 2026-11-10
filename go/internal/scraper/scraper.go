// Package scraper 调用 Python 侧 CLI（jab-scrape）抓取 m3u8 地址。
//
// 接口约定：
//
//	uv run jab-scrape <url> --source jable|missav [-v]
//
// stdout 输出单行 JSON，日志全部走 stderr，exit 0 成功 / 1 失败。
package scraper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"jabgrab/internal/proc"
)

// Result 与 Python 侧 ScrapeResult 对应。
type Result struct {
	OK        bool              `json:"ok"`
	M3U8URL   string            `json:"m3u8_url"`
	VideoName string            `json:"video_name"`
	Headers   map[string]string `json:"headers"`
	Error     string            `json:"error"`
}

// Scraper 按固定参数前缀执行爬取命令。
type Scraper struct {
	Argv    []string      // 命令及其前置参数，如 ["uv","run","--project","/x/python","jab-scrape"]
	Dir     string        // 工作目录
	Timeout time.Duration // 单次爬取超时
	Log     func(string)  // stderr 逐行回调，可为 nil
}

// Scrape 抓取单个 URL。返回的 error 表示流程性故障（进程起不来/超时/无 JSON），
// Result.OK == false 表示爬取本身失败（页面没解析出来）。
func (s Scraper) Scrape(ctx context.Context, url, source string) (Result, error) {
	if len(s.Argv) == 0 {
		return Result{}, fmt.Errorf("爬取命令未配置")
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append(append([]string{}, s.Argv[1:]...), url, "--source", source)
	// 设置了 Dir 之后，带分隔符的相对路径会按新工作目录解析，先转绝对路径；
	// 裸命令名（如 "uv"）保持原样，交给 PATH 查找。
	// Windows 上 "bin/x.exe" 这种正斜杠写法也合法，两种分隔符都要认。
	bin := s.Argv[0]
	if !filepath.IsAbs(bin) && strings.ContainsAny(bin, `/\`) {
		if abs, err := filepath.Abs(bin); err == nil {
			bin = abs
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // 路径来自配置，属预期行为
	cmd.Dir = s.Dir
	proc.Prepare(cmd)
	// CommandContext 默认只杀主进程，而爬取会拉起浏览器等子进程：
	// 必须自己把整组杀掉，否则超时/取消后残留进程还攥着 stdout，Wait 会一直阻塞。
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	cmd.WaitDelay = 5 * time.Second

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("无法读取爬取进程 stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("启动爬取进程失败: %w", err)
	}

	// stderr 可能很长（浏览器日志），实时转发给 UI，同时留最后几行做错误摘要。
	tail := &ring{limit: 20}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			tail.push(line)
			if s.Log != nil {
				s.Log(line)
			}
		}
	}()

	waitErr := cmd.Wait()
	// 先等读协程收尾，再取摘要：否则会与 push 并发，还可能丢掉最后几行错误信息。
	<-stderrDone

	if ctx.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("爬取超时（%s）", timeout)
	}
	if ctx.Err() == context.Canceled {
		return Result{}, context.Canceled
	}

	res, perr := ParseStdout(stdout.Bytes())
	if perr != nil {
		if waitErr != nil {
			if msg := tail.summary(); msg != "" {
				return Result{}, fmt.Errorf("爬取进程异常退出: %v（%s）", waitErr, msg)
			}
			return Result{}, fmt.Errorf("爬取进程异常退出: %w", waitErr)
		}
		return Result{}, perr
	}
	return res, nil
}

// ParseStdout 从 stdout 中取出 JSON 结果。
// 允许 JSON 前后有多余空行，取第一个以 { 开头且能解析的行。
func ParseStdout(b []byte) (Result, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var res Result
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			continue
		}
		return res, nil
	}
	return Result{}, fmt.Errorf("爬取进程没有输出有效 JSON")
}

// ring 保存最后 N 行，用于错误摘要。
type ring struct {
	mu    sync.Mutex
	limit int
	buf   []string
}

func (r *ring) push(s string) {
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
