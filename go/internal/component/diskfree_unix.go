//go:build !windows

package component

import "syscall"

// freeSpace 返回 path 所在文件系统的可用字节数。
//
// 用 Bavail（普通用户可用）而不是 Bfree：后者包含只有 root 能用的保留块，
// 拿它判断会得出「空间够」的结论，随后写入仍然失败。
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
