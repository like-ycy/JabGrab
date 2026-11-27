package proc

import (
	"strings"
	"sync"
)

// Ring 保存最后 N 行输出，用于子进程异常退出时的错误摘要。
// 多个读协程可能并发写入，必须加锁。
type Ring struct {
	mu    sync.Mutex
	limit int
	buf   []string
}

func NewRing(limit int) *Ring { return &Ring{limit: limit} }

func (r *Ring) Push(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, s)
	if len(r.buf) > r.limit {
		r.buf = r.buf[1:]
	}
}

// Summary 返回最后一行（去首尾空白，超长时保留末尾 240 字符）。
func (r *Ring) Summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return ""
	}
	line := strings.TrimSpace(r.buf[len(r.buf)-1])
	if len(line) > 240 {
		line = line[len(line)-240:]
	}
	return line
}
