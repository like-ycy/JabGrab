//go:build windows

package player

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modShell32       = windows.NewLazySystemDLL("shell32.dll")
	procShellExecute = modShell32.NewProc("ShellExecuteW")
)

// swShowNormal 让播放器窗口按正常状态出现（不最小化、不隐藏）。
const swShowNormal = 1

// openDefault 用 ShellExecuteW 交给系统默认关联程序。
//
// 不用 `cmd /c start "" <file>`：cmd 会把文件名里的 & ^ ( ) 当成自己的语法
// 重新解析，带这些字符的片名轻则打不开、重则执行到别的东西；而且 cmd 会闪
// 一下黑窗。ShellExecuteW 走的就是资源管理器双击的那条路。
func openDefault(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}

	ret, _, _ := procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0,
		0,
		uintptr(swShowNormal),
	)
	// 返回值 <= 32 表示失败，且它不是 errno（GetLastError 往往没有意义），
	// 只报返回码，别把无关的 errno 文案当成原因展示给用户。
	if ret <= 32 {
		return fmt.Errorf("系统默认播放器打不开（ShellExecuteW 返回 %d）", ret)
	}
	return nil
}
