//go:build windows

package component

import "golang.org/x/sys/windows"

// freeSpace 返回 path 所在卷的可用字节数。
//
// 取 freeBytesAvailableToCaller（调用者可用）而不是 totalFree：
// 磁盘配额存在时两者不同，用后者会判断成「空间够」，随后写入失败。
func freeSpace(path string) (uint64, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}
