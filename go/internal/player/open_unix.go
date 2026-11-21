//go:build !windows

package player

import (
	"os/exec"
	"runtime"
)

// openDefault 交给系统默认关联程序：macOS 用 open，其它 unix 用 xdg-open。
func openDefault(path string) error {
	if runtime.GOOS == "darwin" {
		return start(exec.Command("open", path))
	}
	return start(exec.Command("xdg-open", path))
}
