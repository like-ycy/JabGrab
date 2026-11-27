// Package curlrelay 用系统 curl 获取 HLS 资源，保留下载器的解析、重试和混流。
package curlrelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxManifestSize = 8 * 1024 * 1024

type Options struct {
	URL         string
	Headers     map[string]string
	Concurrency int
	Timeout     time.Duration
}

type Relay struct {
	cancel    context.CancelFunc
	server    *http.Server
	workers   sync.WaitGroup
	sem       chan struct{}
	bin       string
	headers   http.Header
	timeout   time.Duration
	origin    *url.URL
	base      string
	entry     string
	entryPath string
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	routes    map[string]*url.URL
	byURL     map[string]string
	lastErr   error
}

func Start(ctx context.Context, opts Options) (*Relay, error) {
	bin, err := FindCurl()
	if err != nil {
		return nil, err
	}
	root, err := url.Parse(opts.URL)
	if err != nil || !httpURL(root) {
		return nil, errors.New("curl 媒体源必须是 HTTP(S) URL")
	}
	headers := make(http.Header)
	for key, value := range opts.Headers {
		if strings.ContainsAny(key+value, "\r\n") || strings.ContainsAny(key, " :\t") || key == "" {
			return nil, errors.New("媒体请求头包含无效字符")
		}
		headers.Set(key, value)
	}
	headers.Set("Accept-Encoding", "identity")
	if opts.Concurrency <= 0 {
		opts.Concurrency = 16
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &Relay{cancel: cancel, bin: bin, headers: headers, origin: root,
		timeout: opts.Timeout, sem: make(chan struct{}, opts.Concurrency),
		base:   "http://" + listener.Addr().String() + "/" + rand.Text(),
		routes: make(map[string]*url.URL), byURL: make(map[string]string)}
	r.entry, err = r.register(root)
	if err != nil {
		cancel()
		listener.Close()
		return nil, err
	}
	entryURL, _ := url.Parse(r.entry)
	r.entryPath = entryURL.Path
	r.server = &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = r.server.Serve(listener) }()
	return r, nil
}

func (r *Relay) URL() string { return r.entry }

func (r *Relay) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancel()
		_ = r.server.Close()
		r.workers.Wait()
	})
}

func (r *Relay) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

func httpURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil
}

func surrit(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return (host == "surrit.com" || strings.HasSuffix(host, ".surrit.com")) && (u.Port() == "" || u.Port() == "443" || u.Port() == "80")
}

func (r *Relay) allowed(u *url.URL) bool {
	return httpURL(u) && (strings.EqualFold(u.Host, r.origin.Host) || (surrit(r.origin) && surrit(u)))
}

func (r *Relay) register(u *url.URL) (string, error) {
	if !r.allowed(u) {
		return "", errors.New("HLS 资源或重定向离开了允许的媒体源")
	}
	copyURL := *u
	u = &copyURL
	u.Fragment = ""
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.byURL[u.String()]; existing != "" {
		return existing, nil
	}
	// 路由只接受已从可信入口/清单发现的地址，不接受客户端传入任意远端 URL。
	local := r.base + "/" + strconv.Itoa(len(r.routes))
	if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
		local += ".m3u8"
	}
	path, _ := url.Parse(local)
	r.routes[path.Path] = u
	r.byURL[u.String()] = local
	return local, nil
}

func (r *Relay) fail(w http.ResponseWriter, err error) {
	r.mu.Lock()
	if !r.closed {
		r.lastErr = err
	}
	r.mu.Unlock()
	http.Error(w, "media fetch failed", http.StatusBadGateway)
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Close 先取消 BaseContext 并关闭 server，随后等待所有在途 curl 退出。
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	r.workers.Add(1)
	target := r.routes[req.URL.Path]
	r.mu.Unlock()
	defer r.workers.Done()
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if target == nil || req.URL.IsAbs() || req.URL.RawQuery != "" {
		http.NotFound(w, req)
		return
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-req.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
	defer cancel()
	headers := r.headers.Clone()
	for _, key := range []string{"Range", "If-Range"} {
		if value := req.Header.Get(key); value != "" {
			headers.Set(key, value)
		}
	}
	response, finalURL, err := r.get(ctx, target, req.Method, headers)
	if err != nil {
		r.fail(w, err)
		return
	}
	defer response.close()
	if response.status < 200 || response.status >= 300 {
		r.fail(w, fmt.Errorf("curl 访问媒体源返回 HTTP %d", response.status))
		return
	}
	if req.Method == http.MethodHead {
		_, _ = io.Copy(io.Discard, response.body)
		if err := response.wait(); err != nil {
			r.fail(w, err)
			return
		}
		copyHeaders(w.Header(), response.header)
		w.WriteHeader(response.status)
		return
	}
	prefix, _ := response.body.Peek(10)
	if bytes.HasPrefix(bytes.TrimPrefix(prefix, []byte{0xef, 0xbb, 0xbf}), []byte("#EXTM3U")) {
		body, err := io.ReadAll(io.LimitReader(response.body, maxManifestSize+1))
		if err == nil && len(body) > maxManifestSize {
			err = errors.New("HLS 清单超过大小限制")
		}
		if err == nil {
			err = response.wait()
		}
		if err == nil {
			body, err = r.rewrite(body, finalURL)
		}
		if err != nil {
			r.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		return
	}
	if req.URL.Path == r.entryPath || strings.HasSuffix(strings.ToLower(finalURL.Path), ".m3u8") {
		r.fail(w, errors.New("媒体源未返回有效的 HLS 清单"))
		return
	}
	copyHeaders(w.Header(), response.header)
	w.WriteHeader(response.status)
	_, err = io.Copy(w, response.body)
	if err == nil {
		err = response.wait()
	}
	if err != nil {
		r.mu.Lock()
		if !r.closed {
			r.lastErr = err
		}
		r.mu.Unlock()
		// 不能给不完整分片写入正常的 HTTP 结束标记，下载器必须重试。
		panic(http.ErrAbortHandler)
	}
}

func copyHeaders(dst, src http.Header) {
	for _, key := range []string{"Content-Type", "Content-Range", "Accept-Ranges"} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
	if src.Get("Content-Encoding") == "" && src.Get("Transfer-Encoding") == "" && src.Get("Content-Length") != "" {
		dst.Set("Content-Length", src.Get("Content-Length"))
	}
}

func (r *Relay) get(ctx context.Context, target *url.URL, method string, headers http.Header) (*curlResponse, *url.URL, error) {
	for range 6 {
		if !r.allowed(target) {
			return nil, nil, errors.New("媒体重定向离开了允许的源")
		}
		response, err := fetch(ctx, r.bin, target.String(), method, headers, r.timeout)
		if err != nil {
			return nil, nil, err
		}
		switch response.status {
		case 301, 302, 303, 307, 308:
			location := response.header.Get("Location")
			response.close()
			if location == "" {
				return nil, nil, errors.New("媒体重定向缺少 Location")
			}
			target, err = target.Parse(location)
			if err != nil {
				return nil, nil, err
			}
		default:
			return response, target, nil
		}
	}
	return nil, nil, errors.New("媒体重定向次数过多")
}

var uriAttribute = regexp.MustCompile(`\bURI="([^"]*)"`)

func (r *Relay) rewrite(body []byte, base *url.URL) ([]byte, error) {
	resolve := func(raw string) (string, error) {
		if strings.HasPrefix(raw, "data:") {
			return raw, nil
		}
		if strings.Contains(raw, "{$") {
			return "", errors.New("暂不支持带变量引用的 HLS 资源地址")
		}
		u, err := base.Parse(raw)
		if err != nil {
			return "", err
		}
		return r.register(u)
	}
	lines := strings.Split(string(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var err error
		if strings.HasPrefix(line, "#") {
			line = uriAttribute.ReplaceAllStringFunc(line, func(match string) string {
				if err != nil {
					return match
				}
				var local string
				local, err = resolve(uriAttribute.FindStringSubmatch(match)[1])
				return `URI="` + local + `"`
			})
		} else {
			line, err = resolve(line)
		}
		if err != nil {
			return nil, err
		}
		lines[i] = line
	}
	return []byte(strings.Join(lines, "\n")), nil
}
