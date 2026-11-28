package curlrelay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jabgrab/internal/proc"
)

const (
	batchSize    = 4
	batchWindow  = 8 * time.Millisecond
	resultPrefix = "JABGRAB_CURL_RESULT "
)

// FindCurl 优先使用系统 curl，不依赖应用所在目录或浏览器组件。
func FindCurl() (string, error) {
	path, name := "/usr/bin/curl", "curl"
	if runtime.GOOS == "windows" {
		name = "curl.exe"
		path = filepath.Join(os.Getenv("SystemRoot"), "System32", name)
	}
	if st, err := os.Stat(path); filepath.IsAbs(path) && err == nil && !st.IsDir() {
		return path, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("MissAV 下载需要系统 curl: %w", err)
	}
	return path, nil
}

// Stats 用于核对实际的批处理与连接复用效果，不包含本机 HTTP 请求。
type Stats struct {
	Requests    int64
	Processes   int64
	Connections int64
}

type transport struct {
	ctx                              context.Context
	cancel                           context.CancelFunc
	bin, dir                         string
	queue                            chan *transfer
	slots                            chan struct{}
	done                             chan struct{}
	batches                          sync.WaitGroup
	processes, requests, connections atomic.Int64
}

type transfer struct {
	ctx            context.Context
	target, method string
	headers        http.Header
	timeout        time.Duration
	result         chan transferResult
}

type transferResult struct {
	response *curlResponse
	err      error
}

type curlResponse struct {
	status  int
	header  http.Header
	body    *bufio.Reader
	file    *os.File
	release func()
}

func (r *curlResponse) close() {
	r.file.Close()
	r.release()
}

// batchFiles 的目录需保留到 curl 退出且所有已交付正文关闭之后，兼容 Windows 文件锁。
type batchFiles struct {
	dir  string
	refs atomic.Int32
}

func (f *batchFiles) release() {
	if f.refs.Add(-1) == 0 {
		_ = os.RemoveAll(f.dir)
	}
}

func newTransport(parent context.Context, bin string, concurrency int) (*transport, error) {
	dir, err := os.MkdirTemp("", "jabgrab-curl-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	t := &transport{ctx: ctx, cancel: cancel, bin: bin, dir: dir, queue: make(chan *transfer, concurrency), slots: make(chan struct{}, concurrency), done: make(chan struct{})}
	go t.dispatch()
	return t, nil
}

func (t *transport) close() {
	t.cancel()
	<-t.done
	t.batches.Wait()
	_ = os.RemoveAll(t.dir)
}

func (t *transport) stats() Stats {
	return Stats{Requests: t.requests.Load(), Processes: t.processes.Load(), Connections: t.connections.Load()}
}

func (t *transport) fetch(ctx context.Context, target, method string, headers http.Header, timeout time.Duration) (*curlResponse, error) {
	job := &transfer{ctx: ctx, target: target, method: method, headers: headers, timeout: timeout, result: make(chan transferResult)}
	select {
	case t.queue <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, t.ctx.Err()
	}
	select {
	case result := <-job.result:
		return result.response, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, t.ctx.Err()
	}
}

func (t *transport) deliver(job *transfer, response *curlResponse, err error) {
	select {
	case job.result <- transferResult{response: response, err: err}:
		return
	case <-job.ctx.Done():
	case <-t.ctx.Done():
	}
	if response != nil {
		response.close()
	}
}

func (t *transport) dispatch() {
	defer close(t.done)
	for {
		var jobs []*transfer
		select {
		case job := <-t.queue:
			jobs = append(jobs, job)
		case <-t.ctx.Done():
			return
		}
		timer := time.NewTimer(batchWindow)
	collect:
		for len(jobs) < batchSize {
			select {
			case job := <-t.queue:
				jobs = append(jobs, job)
			case <-timer.C:
				break collect
			case <-t.ctx.Done():
				timer.Stop()
				return
			}
		}
		timer.Stop()
		live := jobs[:0]
		for _, job := range jobs {
			if job.ctx.Err() == nil {
				live = append(live, job)
			}
		}
		jobs = live
		if len(jobs) == 0 {
			continue
		}
		// 每批最多四个请求、两条并发连接，完成一个即交付一个。
		// 至少两轮传输才能复用连接；跨批次的总并发不超过配置值。
		width := min(2, (len(jobs)+1)/2, cap(t.slots))
		for range width {
			select {
			case t.slots <- struct{}{}:
			case <-t.ctx.Done():
				return
			}
		}
		t.batches.Add(1)
		go func(jobs []*transfer, width int) {
			defer t.batches.Done()
			defer func() {
				for range width {
					<-t.slots
				}
			}()
			t.runBatch(jobs, width)
		}(jobs, width)
	}
}

func configValue(s string) string {
	// curl -K 的引号规则只需这五种转义；不经 shell，URL/头值不能生成新选项。
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

func (t *transport) runBatch(jobs []*transfer, width int) {
	completed := make([]bool, len(jobs))
	failure := errors.New("curl 批次未返回完整结果（需要 curl 7.75 或更新版本）")
	defer func() {
		for i, job := range jobs {
			if !completed[i] {
				t.deliver(job, nil, failure)
			}
		}
	}()
	dir, err := os.MkdirTemp(t.dir, "batch-")
	if err != nil {
		failure = err
		return
	}
	files := &batchFiles{dir: dir}
	files.refs.Store(1)
	defer files.release()
	ctx, cancel := context.WithCancel(t.ctx)
	defer cancel()
	var active atomic.Int32
	active.Store(int32(len(jobs)))
	for _, job := range jobs {
		stop := context.AfterFunc(job.ctx, func() {
			if active.Add(-1) == 0 {
				cancel()
			}
		})
		defer stop()
	}
	var config strings.Builder
	for i, job := range jobs {
		if i > 0 {
			config.WriteString("next\n")
		}
		limit := job.timeout
		if deadline, ok := job.ctx.Deadline(); ok {
			limit = min(limit, time.Until(deadline))
		}
		config.WriteString("globoff\nhttp1.1\ncompressed\n")
		// 保留用户对代理继承的设置，不加载 .curlrc；不再强制 --noproxy。
		config.WriteString("proto = \"=http,https\"\n")
		fmt.Fprintf(&config, "max-time = %.3f\n", max(limit.Seconds(), 0.001))
		if job.method == http.MethodHead {
			config.WriteString("head\n")
		}
		keys := make([]string, 0, len(job.headers))
		for k := range job.headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&config, "header = %s\n", configValue(k+": "+job.headers.Get(k)))
		}
		fmt.Fprintf(&config, "url = %s\noutput = %s\ndump-header = %s\n", configValue(job.target), configValue(filepath.Join(dir, fmt.Sprintf("%d.body", i))), configValue(filepath.Join(dir, fmt.Sprintf("%d.head", i))))
		// --no-buffer 只约束正文输出，write-out 的 stdout 仍可能整批缓冲。
		// stderr 不缓冲，保证一个分片结束就能交付，无需等慢分片。
		format := fmt.Sprintf("%%{stderr}%s%d %%{http_code} %%{exitcode} %%{num_connects}\\n", resultPrefix, i)
		fmt.Fprintf(&config, "write-out = %s\n", configValue(format))
	}
	cmd := exec.CommandContext(ctx, t.bin, "-q", "--silent", "--show-error", "--no-buffer", "--parallel", "--parallel-max", strconv.Itoa(width), "--config", "-")
	cmd.Stdin = strings.NewReader(config.String())
	proc.Prepare(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	cmd.WaitDelay = 2 * time.Second
	var diagnostic strings.Builder
	pipe, err := cmd.StderrPipe()
	if err != nil {
		failure = err
		return
	}
	defer pipe.Close()
	if err = cmd.Start(); err != nil {
		failure = fmt.Errorf("启动 curl 失败: %w", err)
		return
	}
	t.processes.Add(1)
	t.requests.Add(int64(len(jobs)))
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, resultPrefix) {
			if diagnostic.Len() < 1000 {
				message := line + "\n"
				diagnostic.WriteString(message[:min(len(message), 1000-diagnostic.Len())])
			}
			continue
		}
		var id, status, code, connections int
		if n, err := fmt.Sscanf(strings.TrimPrefix(line, resultPrefix), "%d %d %d %d", &id, &status, &code, &connections); err != nil || n != 4 || id < 0 || id >= len(jobs) || completed[id] {
			cancel()
			break
		}
		completed[id] = true
		t.connections.Add(int64(connections))
		if code != 0 {
			t.deliver(jobs[id], nil, fmt.Errorf("curl 请求失败: exit %d, HTTP %d", code, status))
			continue
		}
		response, err := readResult(files, id, status)
		t.deliver(jobs[id], response, err)
	}
	if err := scanner.Err(); err != nil {
		cancel() // 不让 curl 阻塞在已停止读取的诊断管道上。
		failure = fmt.Errorf("读取 curl 完成通知失败: %w", err)
	}
	if err = cmd.Wait(); err != nil {
		message := strings.TrimSpace(diagnostic.String())
		if len(message) > 1000 {
			message = message[:1000]
		}
		failure = fmt.Errorf("curl 批次失败: %w (%s)", err, message)
	}
}

func readResult(files *batchFiles, id, status int) (*curlResponse, error) {
	headerFile, err := os.Open(filepath.Join(files.dir, fmt.Sprintf("%d.head", id)))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(headerFile, 64*1024+1))
	headerFile.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > 64*1024 {
		return nil, errors.New("curl 响应头超过大小限制")
	}
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	var headers http.Header
	lastStatus := 0
	for {
		line, err := reader.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
			return nil, errors.New("无效 curl HTTP 状态行")
		}
		lastStatus, err = strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		mime, err := reader.ReadMIMEHeader()
		if err != nil {
			return nil, err
		}
		headers = http.Header(mime)
	}
	if headers == nil || lastStatus != status {
		return nil, errors.New("curl 响应与完成状态不一致")
	}
	file, err := os.Open(filepath.Join(files.dir, fmt.Sprintf("%d.body", id)))
	if err != nil {
		return nil, err
	}
	files.refs.Add(1)
	return &curlResponse{status: status, header: headers, body: bufio.NewReader(file), file: file, release: files.release}, nil
}
