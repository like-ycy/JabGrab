package component

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// lockFileName 是跨进程互斥用的锁文件。
	//
	// 用操作系统文件锁（flock / LockFileEx），不用「mkdir 占位」这类技巧：
	// mkdir 锁在进程崩溃后必须靠 mtime 判断过期，而「判断过期 + 删除 + 重建」
	// 这三步本身不是原子的 —— 两个实例同时判过期，一个刚拿到新锁就可能被另一个
	// 按路径删掉，于是两边都进入了临界区。文件锁由内核在进程退出时自动释放，
	// 没有这个竞态，也省掉了刷新 mtime 的心跳协程。
	lockFileName = ".lock"

	// installLockWait 是等待另一个实例完成安装的上限。
	installLockWait = 5 * time.Minute

	// lockPoll 是抢锁失败后的重试间隔。
	lockPoll = 50 * time.Millisecond
)

// acquireLock 取得组件根目录的跨进程互斥锁，返回释放函数。
//
// 为什么需要它：同一个数据目录可能被两个 App 实例使用，而安装与清理都作用于
// 同一批目录。没有这把锁时，B 的启动清理会把 A 正在下载的 staging 目录当遗留
// 物删掉 —— A 的表现是「下载完成却报 payload.zip 不存在」，而这与网络、
// 磁盘都无关，极难归因。
//
// wait <= 0 表示只尝试一次（清理用）：拿不到就跳过，不该为了清垃圾阻塞启动。
func (m *Manager) acquireLock(ctx context.Context, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, fmt.Errorf("创建组件目录 %s: %w", m.root, err)
	}
	path := filepath.Join(m.root, lockFileName)
	file, err := acquireFileLock(path, wait, lockPoll, ctx)
	if err != nil {
		return nil, err
	}
	return func() { releaseFileLock(file) }, nil
}

// errLockBusy 是「锁被别人持有」的判定结果，各平台实现在自己的文件里返回它。
var errLockBusy = fmt.Errorf("另一个实例正在安装组件，请稍后重试")
