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
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return KillPID(cmd.Process.Pid)
}

// KillPID 终止以 pid 为组长的整个进程组。
//
// 进程已退出（ESRCH）时视为成功，避免 ctx 取消与自然退出竞争时报错。
// 单独暴露成函数是为了让「停止」按钮能直接下手：exec 的取消回调要等调度，
// 而 dl-re 的 ffmpeg 子进程、selenium 拉起的浏览器多活一秒就多写一秒盘。
func KillPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	// 拿不到进程组（还没 Prepare 过、或组已被拆散）时退回只杀这个 pid。
	p, err := os.FindProcess(pid) // unix 上总能拿到句柄，不代表进程还活着
	if err != nil {
		return err
	}
	if kerr := p.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		return kerr
	}
	return nil
}
