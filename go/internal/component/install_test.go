package component

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestInstallStripsTopLevelDir 钉住包内顶层目录被剥掉。
//
// CI 用 `zip -r scraper.zip scraper` 打包，包里天然有一层 `scraper/`。
// 不剥掉它，落盘结构就是 components/scraper-0.4.3/scraper/scraper，
// 与 toolpath 约定的 `<组件目录>/<可执行文件>` 差一层，表现为「装了但找不到」。
func TestInstallStripsTopLevelDir(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))

	path := mustInstall(t, m, spec)

	want := filepath.Join(root, "scraper-0.4.3", exeName("scraper"))
	if path != want {
		t.Fatalf("落盘路径错误：期望 %s，实际 %s", want, path)
	}
	if _, err := os.Stat(filepath.Join(root, "scraper-0.4.3", "_internal", "libpython.dylib")); err != nil {
		t.Fatalf("附属文件未解出：%v", err)
	}
	if got := m.State(spec); got != Ready {
		t.Fatalf("安装后状态应为 Ready，实际 %s", got)
	}
	if got := m.Path(spec.Name); got != path {
		t.Fatalf("Path 返回 %q，期望 %s", got, path)
	}
}

// TestInstallIsIdempotent 钉住「已经是期望版本就不再下载」。
//
// 调用方会在启动时无脑调一次 Install，不能每次启动都重下 20MB。
func TestInstallIsIdempotent(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, hits := serve(t, standardPackage(t))

	first := mustInstall(t, m, spec)
	second := mustInstall(t, m, spec)

	if first != second {
		t.Fatalf("两次安装路径不同：%s vs %s", first, second)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("已安装仍重复下载：命中 %d 次", n)
	}
}

// TestInstallRejectsBadChecksum 确认校验失败时目标目录从未被创建。
//
// 关键不是「报错」，而是「没留下任何东西」：留下半成品目录会让 toolpath
// 找到它、exec 尝试运行它，失败信息指向刮削器本身，与真实原因无关。
func TestInstallRejectsBadChecksum(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	spec.Asset.SHA256 = strings.Repeat("0", 64)

	if _, err := m.Install(t.Context(), spec, nil); err == nil {
		t.Fatal("校验码不符却安装成功")
	} else if !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("错误信息未指出校验失败：%v", err)
	}

	assertNoResidue(t, root)
}

// TestInstallStopsOnCancel 确认下载被取消后不留残余。
func TestInstallStopsOnCancel(t *testing.T) {
	root := testDir(t)
	m := New(root)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 挂住直到客户端断开：模拟慢网下用户点了取消。
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	spec := Spec{
		Name:     "scraper",
		Version:  "0.4.3",
		Protocol: 1,
		Asset:    Asset{URL: srv.URL, Size: 1000000, SHA256: strings.Repeat("a", 64)},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := m.Install(ctx, spec, nil); err == nil {
		t.Fatal("上下文取消后安装仍成功")
	}

	assertNoResidue(t, root)
}

// TestInstallRejectsPathTraversal 确认包内 `../` 不会把文件写到根目录之外。
//
// manifest 来自网络，一个被替换的包就能把文件写到任意位置。
func TestInstallRejectsPathTraversal(t *testing.T) {
	root := testDir(t)
	m := New(root)
	payload := buildZip(t, []zipEntry{
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "../escaped.txt", body: "boom", mode: 0o644},
	})
	spec, _ := serve(t, payload)

	_, err := m.Install(t.Context(), spec, nil)
	if err == nil {
		t.Fatal("越界路径未被拒绝")
	}
	if !strings.Contains(err.Error(), "越界路径") {
		t.Fatalf("错误信息未指出越界：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escaped.txt")); statErr == nil {
		t.Fatal("文件被写到了组件根目录之外")
	}
	assertNoResidue(t, root)
}

// TestInstallRequiresBinary 确认包里没有可执行文件时报错。
//
// 少了这一步，装出来的目录状态是 Ready，直到用户点开始刮削才炸。
func TestInstallRequiresBinary(t *testing.T) {
	root := testDir(t)
	m := New(root)
	payload := buildZip(t, []zipEntry{
		{name: "scraper/_internal/only-data.bin", body: "x", mode: 0o644},
	})
	spec, _ := serve(t, payload)

	if _, err := m.Install(t.Context(), spec, nil); err == nil {
		t.Fatal("缺少可执行文件却安装成功")
	}
	assertNoResidue(t, root)
}

// TestInstallReportsProgress 确认进度回调给到最后的总量。
//
// 前端据此画进度条；只报 0 不报满会让进度条停在 0% 直到下载结束。
func TestInstallReportsProgress(t *testing.T) {
	root := testDir(t)
	m := New(root)
	data := standardPackage(t)
	spec, _ := serve(t, data)

	var done, total int64
	calls := 0
	mustInstallWithProgress(t, m, spec, func(d, tt int64) {
		done, total = d, tt
		calls++
	})

	if calls < 2 {
		t.Fatalf("进度回调次数过少：%d", calls)
	}
	if done != int64(len(data)) || total != int64(len(data)) {
		t.Fatalf("进度终值错误：done=%d total=%d 期望 %d", done, total, len(data))
	}
}

// TestInstallPreservesExecBit 确认可执行权限没被解压流程吃掉。
//
// zip 里存了 0755，解出来变成 0644 的话，macOS/Linux 上 exec 会得到
// permission denied，而 App 侧只会显示「刮削器异常退出」。
func TestInstallPreservesExecBit(t *testing.T) {
	if exeName("x") == "x.exe" {
		t.Skip("Windows 不区分可执行位")
	}
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))

	path := mustInstall(t, m, spec)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("可执行位丢失：%v", info.Mode())
	}
}

// TestCommonTopDir 钉住「包内公共顶层目录」的判定。
//
// 这是踩过的坑：`zip -r scraper.zip scraper` 的第一条是目录条目 `scraper/`，
// 若在判断前去掉尾斜杠，它就变成「没有分隔符的顶层裸条目」，于是被判成
// 「不存在公共目录」，整层目录被原样保留。产物看上去装好了，只是路径多一层，
// 而 App 侧找不到刮削器时只会说「组件未安装」。
func TestCommonTopDir(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		want    string
	}{
		{"目录条目在首位", []string{"scraper/", "scraper/a", "scraper/_internal/b"}, "scraper/"},
		{"只有文件条目", []string{"scraper/a", "scraper/b/c"}, "scraper/"},
		{"顶层裸文件", []string{"a.exe", "scraper/b"}, ""},
		{"多个顶层目录", []string{"a/x", "b/y"}, ""},
		{"单个裸条目", []string{"scraper"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TopLevelDir(zipNames(t, c.entries).File); got != c.want {
				t.Fatalf("TopLevelDir = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestInstallCreatesSymlink 确认包内的相对符号链接按其本意重建。
//
// PyInstaller 的 onedir 产物目前没有符号链接，但 macOS 的 framework 布局
// 一旦出现在组件包里，把链接当普通文件写出来会得到一个内容为路径文本的
// 空壳文件，加载时才报错。
func TestInstallCreatesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要开发者模式或管理员权限")
	}
	root := testDir(t)
	m := New(root)
	payload := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "scraper/_internal/lib.dylib", body: "lib", mode: 0o644},
		{name: "scraper/lib.dylib", body: "_internal/lib.dylib", mode: os.ModeSymlink | 0o777},
	})
	spec, _ := serve(t, payload)
	mustInstall(t, m, spec)

	link := filepath.Join(root, "scraper-0.4.3", "lib.dylib")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("符号链接未创建: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("不是符号链接：%v", info.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("读取链接目标: %v", err)
	}
	if target != "_internal/lib.dylib" {
		t.Fatalf("链接目标错误：%q", target)
	}
}

// TestInstallRejectsAbsoluteSymlink 确认指向绝对路径的链接被拒绝。
//
// 组件包是我们自己 CI 产的，但 manifest 与下载源都在网络上。一个绝对链接
// 能在用户机器上指向任意文件，而它看上去只是包里的一个条目。
func TestInstallRejectsAbsoluteSymlink(t *testing.T) {
	root := testDir(t)
	m := New(root)
	payload := buildZip(t, []zipEntry{
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "scraper/evil", body: "/etc/passwd", mode: os.ModeSymlink | 0o777},
	})
	spec, _ := serve(t, payload)

	_, err := m.Install(t.Context(), spec, nil)
	if err == nil {
		t.Fatal("绝对符号链接未被拒绝")
	}
	if !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("错误信息未指出绝对路径：%v", err)
	}
	assertNoResidue(t, root)
}

func mustInstallWithProgress(t *testing.T, m *Manager, spec Spec, progress ProgressFunc) string {
	t.Helper()
	path, err := m.Install(t.Context(), spec, progress)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	return path
}

// assertNoResidue 断言组件根目录下没有任何版本目录或半成品目录。
func assertNoResidue(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("读取组件目录: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("失败后残留目录：%s", entry.Name())
		}
	}
}
