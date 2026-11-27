//go:build !windows

package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jabgrab/internal/config"
	"jabgrab/internal/proc"
	"jabgrab/internal/scraper"
)

// 「停止」必须真的杀掉在跑的进程组，而不是只发一个取消信号。
//
// 这条用例专门覆盖「取消回调还没被调度」的那条路径：不给 cancel，只靠登记的
// pid 下手。用户报的现象是「点了停止，m3u8_dl_re / 浏览器还在跑」—— 取消是
// 异步的，按下按钮到进程真的退出之间那段窗口必须由这里补上。
func TestStopKillsRegisteredChild(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	proc.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sleep 失败: %v", err)
	}
	pid := cmd.Process.Pid

	gone := make(chan struct{})
	var reaped bool
	reap := func() {
		if reaped {
			return
		}
		reaped = true
		// 用 Process.Wait 而不是 cmd.Wait：这里不关心 stdio，也不该被它拖住。
		_, _ = cmd.Process.Wait()
		close(gone)
	}
	t.Cleanup(func() {
		_ = proc.KillPID(pid)
		reap()
	})

	r := New(config.Default(), Hooks{})
	r.beginChild(pid)
	r.mu.Lock()
	registered := r.children[pid]
	r.mu.Unlock()
	if !registered {
		t.Fatal("beginChild 应记下这个子进程")
	}

	r.Stop() // cancel 为 nil：只有登记的 pid 能救场

	go reap()
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatalf("停止之后进程 %d 仍在运行", pid)
	}
}

// 爬取阶段的子进程（以及它拉起的浏览器）也要能被停止带走。
func TestStopKillsScrapeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	script := filepath.Join(dir, "fake-scraper.sh")
	// 起一个「浏览器」子进程再等它：两个进程在同一个进程组里，
	// 只杀主进程会剩下一代孤儿。
	body := `#!/bin/sh
sleep 300 &
echo "$$ $!" > "$PIDFILE"
wait
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIDFILE", pidFile)

	r := New(config.Default(), Hooks{OnLog: func(string, string) {}})
	r.state = State{Running: true, Total: 1, Items: []Item{
		{URL: "https://jable.tv/videos/abf-403", Site: "jable", Status: StatusPending},
	}}
	r.token = 1
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.running = true

	done := make(chan error, 1)
	go func() {
		_, err := r.scrapePhase(ctx, scraper.Scraper{
			Argv:    []string{script},
			Timeout: 60 * time.Second,
		}, 0, 1)
		done <- err
	}()

	_, grandchild, ok := readPIDs(pidFile, 5*time.Second)
	if !ok {
		t.Fatal("爬取进程没起来（没读到 pid 文件）")
	}

	r.Stop()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("被停止的爬取应返回 context.Canceled，got=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("停止之后爬取阶段没有返回")
	}
	if !waitGone(grandchild, 3*time.Second) {
		t.Fatalf("停止之后子进程 %d 还在运行", grandchild)
	}
}

// readPIDs 等 pid 文件出现并读出「脚本 pid 与孙进程 pid」。
func readPIDs(path string, wait time.Duration) (int, int, bool) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if fields := strings.Fields(string(data)); len(fields) == 2 {
				a, e1 := strconv.Atoi(fields[0])
				b, e2 := strconv.Atoi(fields[1])
				if e1 == nil && e2 == nil {
					return a, b, true
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, 0, false
}

// waitGone 等进程彻底消失（僵尸也算「还在」，所以只认 ESRCH）。
func waitGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
