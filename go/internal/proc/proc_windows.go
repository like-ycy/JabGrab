//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// Prepare 让后台命令不创建控制台，同时保留独立进程组。
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// Kill 用 taskkill /T /F 递归终止进程树，Windows 下没有进程组信号可用。
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return KillPID(cmd.Process.Pid)
}

// KillPID 递归终止以 pid 为根的整棵进程树。
//
// 暴露成函数是为了让「停止」按钮能直接下手，不必等 exec 的取消回调被调度。
func KillPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)) //nolint:gosec // pid 来自本进程启动的子进程
	Prepare(kill)
	if err := kill.Run(); err != nil {
		p, ferr := os.FindProcess(pid)
		if ferr != nil {
			// 进程已经不在了：与 unix 上的 ESRCH 同义，视为成功，
			// 免得「自然退出」与「按了停止」撞在一起时报一个假失败。
			return nil
		}
		if kerr := p.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			return kerr
		}
	}
	return nil
}
