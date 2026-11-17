//go:build windows

package component

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// acquireFileLock 用 LockFileEx 取得排他锁。
//
// 锁的范围取 1 字节而不是整个文件：LockFileEx 的「冲突」判定按字节区间计算，
// 长度为 0 表示「到文件末尾」，文件为空时这种表达没有意义，两个实例可能都成功。
// 固定锁第 0 个字节就没有这个歧义。
func acquireFileLock(path string, wait, poll time.Duration, ctx context.Context) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开组件锁 %s: %w", path, err)
	}

	deadline := time.Now().Add(wait)
	for {
		overlapped := new(windows.Overlapped)
		err := windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, 1, 0, overlapped,
		)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("获取组件锁 %s: %w", path, err)
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, errLockBusy
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("等待组件锁被取消: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

func releaseFileLock(file *os.File) {
	overlapped := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
	_ = file.Close()
}
