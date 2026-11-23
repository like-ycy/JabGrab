// Package reveal 在系统文件管理器里打开成品视频所在的目录。
//
// 与 player 一样属于「尽力而为」的动作：目录打不开不该影响任务状态，
// 调用方拿到 error 只记一条日志。
package reveal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"jabgrab/internal/proc"
)

// Dir 打开 file 所在的目录。
//
// 文件还在时顺带在文件管理器里选中它（访达 / 资源管理器的标准行为）；
// 文件已经被手动删掉或改名时只打开目录本身 —— 按钮点下去总要有反应，
// 不能因为「选不中那一行」就什么都不发生。
func Dir(file string) error {
	path := strings.TrimSpace(file)
	if path == "" {
		return errors.New("没有可打开的目录")
	}
	// 相对路径会按 wails 进程的工作目录解析，转成绝对路径再交出去。
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	dir := filepath.Dir(path)
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("目录打不开: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s 不是目录", dir)
	}

	if _, err := os.Stat(path); err == nil {
		return revealFile(path)
	}
	return openDir(dir)
}

// start 启动命令但不等待它退出。
//
// 文件管理器是长驻进程，等下去会把界面线程拖住；但必须回收，否则 unix 上
// 会留僵尸进程。进程组独立，JabGrab 退出时窗口照常开着。
func start(cmd *exec.Cmd) error {
	proc.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打不开文件管理器: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
