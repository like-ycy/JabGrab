//go:build !windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Prepare 让子进程进入独立进程组。
// dl-re 与 Chromium 都会派生子进程，按进程组杀才能清干净。
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Kill 终止 cmd 所在进程组。
// 进程已退出（ESRCH）时视为成功，避免 ctx 取消与自然退出竞争时报错。
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		return kerr
	}
	return nil
}
