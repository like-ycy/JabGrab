package component

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxManifestBytes 限制清单体积。
//
// 清单只有几百字节。解出 200MB 的响应再报「格式不对」等于把内存交给服务器，
// 而下载地址恰恰可能来自一个被篡改的镜像。
const maxManifestBytes = 1 << 20

// NewHTTPClient 返回一个「只在安全地址上跟随重定向」的客户端。
//
// 为什么必须管重定向
// ------------------
// 只校验初始地址是不够的：一个 https 的清单地址可以 302 到
// `http://内网地址/...`，默认客户端会照跟，于是明文内容被当成清单使用 ——
// 中间人因此能同时替换下载地址与 sha256，「校验通过」又变得毫无意义。
// 重定向目标逐个过 ValidateURL，把这条路堵在客户端层，而不是依赖调用方记得
// 每次自己检查。
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("重定向次数过多")
			}
			if err := ValidateURL(req.URL.String()); err != nil {
				return fmt.Errorf("重定向到不安全的地址: %w", err)
			}
			return nil
		},
	}
}

// FetchManifest 从 Release 拉取组件清单。
//
// mirror 与下载用的是同一套前缀语义：镜像服务通常对「任意 GitHub 地址」生效，
// 只给包加速而不给清单加速的话，用户点下载时第一步就失败了。
func FetchManifest(ctx context.Context, client *http.Client, url, mirror string) (Manifest, error) {
	full := ApplyMirror(mirror, url)
	// 清单地址同样要过 https 校验：mirror 可能来自环境变量或用户输入，
	// 明文地址下清单与包都能被改写，校验码就失去了意义。
	if err := ValidateURL(full); err != nil {
		return Manifest{}, fmt.Errorf("组件清单地址不可用: %w", err)
	}
	if client == nil {
		client = NewHTTPClient(20 * time.Second)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("构造组件清单请求: %w", err)
	}
	// 不加这个头时，某些镜像会返回 HTML 引导页而不是文件内容，
	// 症状是「清单解析失败」，看起来像格式问题。
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("下载组件清单: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("下载组件清单：HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("读取组件清单: %w", err)
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, errors.New("组件清单过大，已中止")
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// ApplyMirror 在设置了镜像前缀时改写地址。
//
// 镜像按「前缀」语义处理（`<mirror>/https://github.com/...`），这是 ghproxy
// 一类加速服务的通行做法。目标用户里有一批拿不到 GitHub 直连，没有这条，
// 按需下载对他们就等于功能不可用。
func ApplyMirror(mirror, url string) string {
	mirror = strings.TrimSpace(mirror)
	if mirror == "" {
		return url
	}
	return strings.TrimSuffix(mirror, "/") + "/" + url
}
