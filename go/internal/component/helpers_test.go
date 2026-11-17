package component

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// testDir 在模块根下建一个临时目录，测试结束即删。
//
// 不用 t.TempDir()：它建在系统临时目录下，在受限环境（沙箱、加固的 CI 容器）
// 里写入可能被拒绝，测试会以 error 收场而不是给出真实结论。与 toolpath、
// library 包的测试保持一致。
func testDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join("..", "..", ".gocache-test")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("创建测试目录 %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, "component-")
	if err != nil {
		t.Fatalf("创建临时目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

// buildZip 造一个组件包。
//
// 条目顺序与 `zip -r` 一致：先写顶层目录条目（`scraper/`），再写文件。
// 这个顺序曾经把 TopLevelDir 骗过去（去掉尾斜杠后目录条目变成「顶层裸条目」），
// 结果是整层目录被原样保留 —— 所以样板包必须包含它。
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		header.SetMode(e.mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatalf("写入 zip 条目 %s: %v", e.name, err)
		}
		if e.body == "" && strings.HasSuffix(e.name, "/") {
			continue
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("写入 zip 内容 %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip: %v", err)
	}
	return buf.Bytes()
}

// standardPackage 是「一个能用的 scraper 包」：可执行文件 + _internal 里的附属文件。
func standardPackage(t *testing.T) []byte {
	t.Helper()
	return buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "#!/bin/sh\necho hi\n", mode: 0o755},
		{name: "scraper/_internal/libpython.dylib", body: strings.Repeat("x", 4096), mode: 0o644},
		{name: "scraper/_internal/seleniumbase/base.py", body: "pass\n", mode: 0o644},
	})
}

// zipNames 造一个只有名字与模式的 zip，用来单独测 TopLevelDir。
func zipNames(t *testing.T, names []string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if strings.HasSuffix(name, "/") {
			header.SetMode(os.ModeDir | 0o755)
		} else {
			header.SetMode(0o644)
		}
		if _, err := zw.CreateHeader(header); err != nil {
			t.Fatalf("写入 zip 条目 %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("打开 zip: %v", err)
	}
	return zr
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// serve 起一个只回同一份字节的下载服务，返回 Spec 与命中次数。
func serve(t *testing.T, data []byte) (Spec, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("下载请求未声明 identity 编码，实际 %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	return Spec{
		Name:     "scraper",
		Version:  "0.4.3",
		Protocol: 1,
		Asset: Asset{
			URL:    srv.URL + "/scraper.zip",
			Size:   int64(len(data)),
			SHA256: sha256Hex(data),
		},
	}, &hits
}

func mustInstall(t *testing.T, m *Manager, spec Spec) string {
	t.Helper()
	path, err := m.Install(t.Context(), spec, nil)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	return path
}

// readFileString 读文件，失败即 Fatal。
func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(data)
}
