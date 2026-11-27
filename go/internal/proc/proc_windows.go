//go:build windows

package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

type treeTermination struct {
	done chan struct{}
	err  error
}

var terminations = struct {
	sync.Mutex
	active map[int]*treeTermination
}{active: make(map[int]*treeTermination)}

// Prepare 让后台命令不创建控制台，同时保留独立进程组。
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// Kill 用 taskkill /T /F 递归终止进程树，Windows 下没有进程组信号可用。
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return KillPID(cmd.Process.Pid)
}

// KillPID 递归终止以 pid 为根的整棵进程树。
//
// 暴露成函数是为了让「停止」按钮能直接下手，不必等 exec 的取消回调被调度。
func KillPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	// Stop 与 CommandContext 的取消回调可能同时到达。同一棵树只执行一次
	// taskkill；第二位调用者等待同一个结果，避免重复终止造成拒绝访问误报。
	terminations.Lock()
	if active := terminations.active[pid]; active != nil {
		terminations.Unlock()
		<-active.done
		return active.err
	}
	op := &treeTermination{done: make(chan struct{})}
	terminations.active[pid] = op
	terminations.Unlock()

	op.err = terminateTree(pid)
	terminations.Lock()
	delete(terminations.active, pid)
	close(op.done)
	terminations.Unlock()
	return op.err
}

func terminateTree(pid int) error {
	// 持有只用于等待的句柄：不申请终止/全访问权限，也避免检查期间 PID 被复用。
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // PID 已不存在。
	}
	if err != nil {
		return fmt.Errorf("检查进程 %d 状态失败: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	children, inspectErr := childProcessHandles(uint32(pid))
	defer func() {
		for _, child := range children {
			windows.CloseHandle(child)
		}
	}()
	allExited := func() bool {
		if inspectErr != nil {
			return false // 未能检查某个子进程时，不能把父进程退出当作整树退出。
		}
		for _, h := range append([]windows.Handle{handle}, children...) {
			if exited, err := processExited(h, 0); err != nil || !exited {
				return false
			}
		}
		return true
	}
	if allExited() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)) //nolint:gosec // pid 来自本进程启动的子进程
	Prepare(kill)
	kill.WaitDelay = time.Second
	output, killErr := kill.CombinedOutput()
	if killErr != nil {
		// Windows 对已终止但仍有句柄的进程也会返回 Access denied。
		// 只有内核确认退出才算完成，绝不把实际的权限错误一概忽略。
		deadline := time.Now().Add(time.Second)
		for {
			if allExited() {
				return nil
			}
			if inspectErr != nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if ctx.Err() != nil {
			killErr = ctx.Err()
		}
		// 不退回只杀父进程：那会留下浏览器/ffmpeg，同时丢失 taskkill 的原因。
		return fmt.Errorf("终止进程树 %d 失败: %w; taskkill: %s", pid, errors.Join(killErr, inspectErr), decodeTaskkillOutput(output, 1))
	}
	return nil
}

// CREATE_NO_WINDOW 下系统命令通常输出 OEM 代码页（CP_OEMCP=1），不是 UTF-8。
// 不改变全局控制台代码页；只在接入 UTF-8 日志前解码本次命令的原始字节。
func decodeTaskkillOutput(raw []byte, codePage uint32) string {
	if utf8.Valid(raw) {
		return strings.TrimSpace(string(raw))
	}
	n, err := windows.MultiByteToWideChar(codePage, 0, &raw[0], int32(len(raw)), nil, 0)
	if err != nil {
		return fmt.Sprintf("无法解码代码页 %d 的命令输出: %x", codePage, raw)
	}
	wide := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(codePage, 0, &raw[0], int32(len(raw)), &wide[0], n); err != nil {
		return fmt.Sprintf("无法解码代码页 %d 的命令输出: %x", codePage, raw)
	}
	return strings.TrimSpace(windows.UTF16ToString(wide))
}

// childProcessHandles 保存停止前的后代句柄。失败时也必须确认这些后代已经退出，
// 否则 taskkill 部分成功、只退出父进程时会掩盖残留浏览器的权限错误。
func childProcessHandles(root uint32) ([]windows.Handle, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("读取进程树失败: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	byParent := make(map[uint32][]uint32)
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		byParent[entry.ParentProcessID] = append(byParent[entry.ParentProcessID], entry.ProcessID)
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("枚举进程树失败: %w", err)
	}
	var handles []windows.Handle
	var inspectErr error
	queue := []uint32{root}
	seen := map[uint32]bool{root: true}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range byParent[parent] {
			if seen[pid] {
				continue
			}
			seen[pid] = true
			queue = append(queue, pid)
			h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue
			}
			if err != nil {
				inspectErr = errors.Join(inspectErr, fmt.Errorf("检查子进程 %d 失败: %w", pid, err))
				continue
			}
			handles = append(handles, h)
		}
	}
	return handles, inspectErr
}

func processExited(handle windows.Handle, waitMS uint32) (bool, error) {
	status, err := windows.WaitForSingleObject(handle, waitMS)
	if err != nil {
		return false, fmt.Errorf("等待进程退出失败: %w", err)
	}
	return status == windows.WAIT_OBJECT_0, nil
}
