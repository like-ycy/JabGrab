//go:build windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 使用测试可执行文件模拟爬取器和它派生的浏览器，不依赖 Chrome 或网络。
func TestWindowsProcHelper(t *testing.T) {
	role := os.Getenv("JABGRAB_PROC_TEST_ROLE")
	if role == "" {
		return
	}
	if role == "parent" {
		child := helperCommand(t, "child")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("JABGRAB_PROC_TEST_CHILD"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		_ = child.Wait()
		os.Exit(0)
	}
	if role == "exit" {
		os.Exit(0)
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func helperCommand(t *testing.T, role string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestWindowsProcHelper$")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "JABGRAB_PROC_TEST_ROLE=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "JABGRAB_PROC_TEST_ROLE="+role)
	Prepare(cmd)
	return cmd
}

func TestWindowsKillConcurrentTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("JABGRAB_PROC_TEST_CHILD", pidFile)
	cmd := helperCommand(t, "parent")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(cmd); _ = cmd.Wait() })
	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(pidFile)
		childPID, _ = strconv.Atoi(string(data))
		if childPID > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("未启动模拟浏览器")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(childPID))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := KillPID(cmd.Process.Pid); err != nil {
				t.Errorf("并发停止失败: %v", err)
			}
		})
	}
	wg.Wait()
	if gone, err := processExited(h, 3000); err != nil || !gone {
		t.Fatalf("模拟浏览器仍在运行: gone=%v err=%v", gone, err)
	}
}

func TestWindowsKillAlreadyExitedWithLiveHandle(t *testing.T) {
	cmd := helperCommand(t, "exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// 保留 h，模拟 Windows 上已终止但对象尚未释放的进程。
	if err := KillPID(cmd.Process.Pid); err != nil {
		t.Fatalf("已退出进程不应报拒绝访问: %v", err)
	}
}

func TestWindowsKillFailureKeepsDiagnostic(t *testing.T) {
	cmd := helperCommand(t, "child")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	// 让 taskkill 无法执行：仍存活的目标不能被误判为「已经停止」。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "taskkill.exe"), []byte("invalid executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	err = KillPID(cmd.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), "taskkill:") {
		t.Fatalf("应保留终止进程树的失败原因: %v", err)
	}
	if gone, err := processExited(h, 0); err != nil || gone {
		t.Fatalf("不得以只杀父进程的回退冒充进程树成功: gone=%v err=%v", gone, err)
	}
}

func TestDecodeTaskkillOutput(t *testing.T) {
	{
		raw := []byte{0xb3, 0xc9, 0xb9, 0xa6, 0x3a, 0x20, 0xd2, 0xd1, 0xd6, 0xd5, 0xd6, 0xb9, 0xbd, 0xf8, 0xb3, 0xcc, 0xa1, 0xa3}
		want := "成功: 已终止进程。"
		if got := decodeTaskkillOutput(raw, 936); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	{
		raw := []byte{0x8e, 0xe8, 0xa8, 0xa1, 0xaa, 0xa0, 0x3a, 0x20, 0xaf, 0xe0, 0xae, 0xe6, 0xa5, 0xe1, 0xe1, 0x20, 0xe3, 0xa6, 0xa5, 0x20, 0xa7, 0xa0, 0xa2, 0xa5, 0xe0, 0xe8, 0xf1, 0xad, 0x2e}
		want := "Ошибка: процесс уже завершён."
		if got := decodeTaskkillOutput(raw, 866); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	{
		raw := []byte{0xe6, 0x88, 0x90, 0xe5, 0x8a, 0x9f, 0x3a, 0x20, 0xe5, 0xb7, 0xb2, 0xe7, 0xbb, 0x88, 0xe6, 0xad, 0xa2, 0xe8, 0xbf, 0x9b, 0xe7, 0xa8, 0x8b, 0xe3, 0x80, 0x82}
		want := "成功: 已终止进程。"
		if got := decodeTaskkillOutput(raw, 1); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	{
		raw := []byte{0x53, 0x55, 0x43, 0x43, 0x45, 0x53, 0x53, 0x3a, 0x20, 0x70, 0x72, 0x6f, 0x63, 0x65, 0x73, 0x73, 0x20, 0x74, 0x65, 0x72, 0x6d, 0x69, 0x6e, 0x61, 0x74, 0x65, 0x64, 0x2e}
		want := "SUCCESS: process terminated."
		if got := decodeTaskkillOutput(raw, 1); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if got := decodeTaskkillOutput(nil, 1); got != "" {
		t.Fatalf("empty output: %q", got)
	}
}
