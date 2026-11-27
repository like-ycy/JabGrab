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
