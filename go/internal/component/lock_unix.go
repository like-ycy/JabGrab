//go:build !windows

package component

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquireFileLock 用 flock 取得排他锁。
//
// LOCK_NB 非阻塞，拿不到就自己轮询：这样等待期间仍然可被 ctx 取消，
// 而 flock 的阻塞模式没法响应取消。
//
// 锁随文件描述符存在。进程被杀时内核会回收 fd 并释放锁 —— 这正是它比
// 「mkdir 占位 + mtime 过期」可靠的地方：没有需要人工清理的残留状态。
func acquireFileLock(path string, wait, poll time.Duration, ctx context.Context) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开组件锁 %s: %w", path, err)
	}

	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
