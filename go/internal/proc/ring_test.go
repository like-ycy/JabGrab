package proc

import (
	"sync"
	"testing"
)

// stdout 与 stderr 各一个协程写同一个 Ring，必须在 -race 下无告警。
func TestRingConcurrentPush(t *testing.T) {
	r := NewRing(8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.Push("line")
			}
		}()
	}
	wg.Wait()
	if n := len(r.buf); n != 8 {
		t.Fatalf("ring 应保持 limit 条，实际 %d", n)
	}
	if r.Summary() == "" {
		t.Fatal("摘要不该为空")
	}
}

func TestRingSummary(t *testing.T) {
	r := NewRing(2)
	if r.Summary() != "" {
		t.Fatal("空 ring 摘要应为空")
	}
	r.Push("  ")
	r.Push("first")
	r.Push("last")
	if r.Summary() != "last" {
		t.Fatalf("应取最后一行: %q", r.Summary())
	}
}