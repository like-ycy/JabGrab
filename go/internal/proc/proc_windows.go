//go:build windows

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Prepare 为子进程新建进程组，避免 Ctrl-C 等信号直接打断整组。
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// Kill 用 taskkill /T /F 递归终止进程树，Windows 下没有进程组信号可用。
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	kill := exec.Command("taskkill", "/T", "/F", "/PID", pid)
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
