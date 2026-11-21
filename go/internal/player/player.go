// Package player 把成品视频交给播放器：配了自定义播放器就用它，
// 否则交给系统默认关联程序。
//
// 打开播放器是「尽力而为」的动作：播放器起不来不该让已经下完的任务变成失败，
// 所以调用方拿到 error 只记一条日志。
package player

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"jabgrab/internal/proc"
)

// Open 用 custom 打开 file。custom 为空时交给系统默认关联程序。
func Open(custom, file string) error {
	path := strings.TrimSpace(file)
	if path == "" {
		return errors.New("没有可播放的文件")
	}
	// 相对路径会按 wails 进程的工作目录解析，转成绝对路径再交出去。
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("文件打不开: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("%s 是目录，不是视频文件", path)
	}

	// 播放器路径允许带环境变量（%ProgramFiles%、$HOME 之类），也允许带引号。
	exe := CleanPath(custom)
	if exe == "" {
		return openDefault(path)
	}
	return launch(exe, path)
}

// launch 用自定义播放器打开 file。
func launch(exe, file string) error {
	// macOS 的播放器常以 .app 目录形式存在，直接 exec 目录会报权限错误。
	if runtime.GOOS == "darwin" && strings.EqualFold(filepath.Ext(exe), ".app") {
		return start(exec.Command("open", "-a", exe, file))
	}
	st, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("播放器不可用: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("播放器路径是目录: %s", exe)
	}
	return start(exec.Command(exe, file))
}

// start 启动命令但不等待它退出。
//
// 播放器是长驻进程，等下去会把下载流程拖住；但必须回收，否则 unix 上会留
// 僵尸进程。进程组独立，JabGrab 退出时播放器照常播完。
func start(cmd *exec.Cmd) error {
	proc.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动播放器失败: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
