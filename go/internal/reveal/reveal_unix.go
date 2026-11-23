//go:build !windows

package reveal

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// revealFile 打开 file 所在目录并选中它。
//
// macOS 用 `open -R`（等价于访达里的「显示简介」旁边那条「在访达中显示」）；
// 其它 unix 没有统一的「选中某个文件」入口，退化成打开所在目录。
func revealFile(path string) error {
	if runtime.GOOS == "darwin" {
		return start(exec.Command("open", "-R", path))
	}
	return openDir(filepath.Dir(path))
}

// openDir 只打开目录本身。
func openDir(dir string) error {
	if runtime.GOOS == "darwin" {
		return start(exec.Command("open", dir))
	}
	return start(exec.Command("xdg-open", dir))
}
