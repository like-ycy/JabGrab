//go:build windows

package reveal

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modShell32       = windows.NewLazySystemDLL("shell32.dll")
	procShellExecute = modShell32.NewProc("ShellExecuteW")
)

// swShowNormal 让资源管理器窗口按正常状态出现（不最小化、不隐藏）。
const swShowNormal = 1

// revealFile 打开文件所在目录并选中它。
//
// 参数串自己拼，只有一处要管：「/select,」与路径拼在同一个参数里，路径两侧
// 各一个引号，让含空格的片名不被拆开。不走 `explorer.exe /select,`：那条路
// 要经 cmd 或 os/exec 重新拼一遍命令行，片名里的 & ^ ( ) 会被当成语法重新
// 解析，轻则定位错文件，重则执行到别的东西。
func revealFile(path string) error {
	return shellExecute("explorer.exe", `/select,"`+path+`"`)
}

// openDir 只打开目录本身。
func openDir(dir string) error {
	return shellExecute(dir, "")
}

// shellExecute 交给资源管理器打开 file，params 原样传给被调程序。
//
// 不等待退出：explorer 把请求转给已有窗口后立刻返回，而且它成功时也常常
// 返回 1，看退出码只会得到假失败。
func shellExecute(file, params string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return err
	}
	var p *uint16
	if params != "" {
		if p, err = windows.UTF16PtrFromString(params); err != nil {
			return err
		}
	}

	ret, _, _ := procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(swShowNormal),
	)
	// 返回值 <= 32 表示失败，且它不是 errno（GetLastError 往往没有意义），
	// 只报返回码，别把无关的 errno 文案当成原因展示给用户。
	if ret <= 32 {
		return fmt.Errorf("资源管理器打不开（ShellExecuteW 返回 %d）", ret)
	}
	return nil
}
