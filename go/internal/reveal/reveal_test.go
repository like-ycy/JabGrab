package reveal

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeOpener 把 open / xdg-open 换成一个只记录参数的脚本，返回记录文件的路径。
// 这样测的是「命令行拼对没有」，而不是真的把访达弹到用户脸上。
func fakeOpener(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 走 ShellExecuteW，没有可替换的外部命令")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "args.txt")
	name := "open"
	if runtime.GOOS != "darwin" {
		name = "xdg-open"
	}
	// 记录文件路径加引号：TempDir 在部分系统上带空格。
	body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + record + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return record
}

func waitArgs(t *testing.T, record string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(record); err == nil && len(data) > 0 {
			return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("文件管理器没有跑起来：没等到记录的参数")
	return nil
}

// 文件还在时要「打开所在目录并选中它」：macOS 是 open -R，其它 unix 只开目录。
func TestDirRevealsExistingFile(t *testing.T) {
	record := fakeOpener(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "片名 with space.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Dir(file); err != nil {
		t.Fatalf("打开目录失败: %v", err)
	}
	args := waitArgs(t, record)

	if runtime.GOOS == "darwin" {
		if len(args) != 2 || args[0] != "-R" || args[1] != file {
			t.Fatalf("传给访达的参数不对: %q", args)
		}
		return
	}
	if len(args) != 1 || args[0] != dir {
		t.Fatalf("xdg-open 收到的目录不对: %q", args)
	}
}

// 成品被用户手动删掉 / 改名之后，按钮仍要能打开那一层目录，而不是点了没反应。
func TestDirOpensFolderWhenFileGone(t *testing.T) {
	record := fakeOpener(t)
	dir := t.TempDir()

	if err := Dir(filepath.Join(dir, "gone.mp4")); err != nil {
		t.Fatalf("文件不在了也应当打开目录: %v", err)
	}
	args := waitArgs(t, record)
	if len(args) != 1 || args[0] != dir {
		t.Fatalf("应当只打开目录，实际参数 %q", args)
	}
}

func TestDirRejectsBadInput(t *testing.T) {
	if err := Dir(""); err == nil {
		t.Fatal("空路径应报错")
	}
	if err := Dir(filepath.Join(t.TempDir(), "no-such-dir", "a.mp4")); err == nil {
		t.Fatal("目录不存在应报错")
	}
}
