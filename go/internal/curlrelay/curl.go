package curlrelay

import (
	"bufio"
	"bytes"
	"context"
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
	"time"

	"jabgrab/internal/proc"
)

// FindCurl 优先使用系统 curl，不依赖应用所在目录或浏览器组件。
func FindCurl() (string, error) {
	path := "/usr/bin/curl"
	name := "curl"
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

type curlResponse struct {
	status int
	header http.Header
	body   *bufio.Reader
	pipe   io.ReadCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr bytes.Buffer
	once   sync.Once
	err    error
}

func (r *curlResponse) wait() error {
	r.once.Do(func() {
		r.err = r.cmd.Wait()
		if r.err != nil {
			message := strings.TrimSpace(r.stderr.String())
			if len(message) > 1000 {
				message = message[:1000]
			}
			r.err = fmt.Errorf("curl 请求失败: %w (%s)", r.err, message)
		}
	})
	return r.err
}

func (r *curlResponse) close() {
	r.cancel()
	r.pipe.Close()
	_ = r.wait()
}

func fetch(ctx context.Context, bin, target, method string, headers http.Header, timeout time.Duration) (*curlResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	r := &curlResponse{cancel: cancel}
	// 与 Windows 上验证通过的 curl 请求一致。TUN 仍由系统路由接管；
	// 不额外套系统/环境代理，也不加载 .curlrc 改写这次请求。
	args := []string{"-q", "--silent", "--show-error", "--globoff", "--http1.1",
		"--noproxy", "*", "--include", "--no-buffer", "--compressed",
		"--proto", "=http,https", "--max-time", strconv.FormatFloat(timeout.Seconds(), 'f', 3, 64)}
	if method == http.MethodHead {
		args = append(args, "--head")
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-H", key+": "+headers.Get(key))
	}
	args = append(args, "--", target)
	r.cmd = exec.CommandContext(ctx, bin, args...)
	proc.Prepare(r.cmd)
	r.cmd.Cancel = func() error { return proc.Kill(r.cmd) }
	r.cmd.WaitDelay = 2 * time.Second
	r.cmd.Stderr = &r.stderr
	pipe, err := r.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	r.pipe = pipe
	if err := r.cmd.Start(); err != nil {
		cancel()
		pipe.Close()
		return nil, fmt.Errorf("启动 curl 失败: %w", err)
	}
	// curl 已解码 chunked/compressed 正文，因此这里只解析响应头，不能让
	// http.ReadResponse 再按 Transfer-Encoding/Content-Length 解码一遍。
	limited := &io.LimitedReader{R: pipe, N: 64 * 1024}
	r.body = bufio.NewReader(limited)
	reader := textproto.NewReader(r.body)
	for range 10 {
		line, readErr := reader.ReadLine()
		if readErr != nil {
			r.close()
			return nil, fmt.Errorf("读取 curl 响应失败: %v; %v", readErr, r.err)
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
			r.close()
			return nil, fmt.Errorf("curl 返回了无效 HTTP 状态行")
		}
		r.status, err = strconv.Atoi(fields[1])
		if err != nil || r.status < 100 || r.status > 599 {
			r.close()
			return nil, fmt.Errorf("curl 返回了无效 HTTP 状态码")
		}
		mime, err := reader.ReadMIMEHeader()
		if err != nil {
			r.close()
			return nil, fmt.Errorf("读取 curl 响应头失败: %w", err)
		}
		r.header = http.Header(mime)
		if r.status >= 200 {
			limited.N = 1<<63 - 1
			return r, nil
		}
	}
	r.close()
	return nil, fmt.Errorf("curl 临时响应次数过多")
}
