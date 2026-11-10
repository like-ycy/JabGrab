package downloader

import (
	"context"
	"os"
	"testing"
	"time"
)

// 真实下载端到端探针，默认跳过：
//
//	DLR_E2E=1 go test ./internal/downloader -run TestE2EProbe -v
func TestE2EProbe(t *testing.T) {
	if os.Getenv("DLR_E2E") == "" {
		t.Skip("设置 DLR_E2E=1 才运行真实下载")
	}

	opts := Options{
		Bin:        "../../third_party/N_m3u8DL-RE",
		SaveDir:    t.TempDir(),
		SaveName:   "probe",
		Threads:    8,
		RetryCount: 2,
		TimeoutSec: 20,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	var last Progress
	var lines int
	err := opts.Run(ctx, "https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8",
		func(p Progress) {
			last = p
			t.Logf("进度: %d/%d %.2f%% %s/%s %s eta=%s", p.Current, p.Total, p.Percent, p.Downloaded, p.TotalSize, p.Speed, p.ETA)
		},
		func(line string) {
			lines++
			if lines <= 15 {
				t.Logf("日志: %s", line)
			}
		},
	)
	t.Logf("结束: err=%v 日志行数=%d", err, lines)
	t.Logf("最后进度: %+v", last)

	if last.Total == 0 {
		t.Fatalf("没有解析到任何进度，日志行数=%d", lines)
	}
	if last.Current <= 0 || last.Percent <= 0 {
		t.Fatalf("进度数值不合理: %+v", last)
	}
}
