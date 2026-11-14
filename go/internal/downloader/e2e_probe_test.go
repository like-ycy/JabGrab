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
	var frames int
	var firstAt time.Duration
	start := time.Now()
	err := opts.Run(ctx, "https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8",
		func(p Progress) {
			if frames == 0 {
				firstAt = time.Since(start)
			}
			frames++
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
	total := time.Since(start)
	t.Logf("结束: err=%v 日志行数=%d 进度回调=%d 首帧=%s 总耗时=%s", err, lines, frames, firstAt, total)
	t.Logf("最后进度: %+v", last)

	if last.Total == 0 {
		t.Fatalf("没有解析到任何进度，日志行数=%d", lines)
	}
	if last.Current <= 0 || last.Percent <= 0 {
		t.Fatalf("进度数值不合理: %+v", last)
	}

	// 回归守卫：重定向下 dl-re 的进度帧不带任何 \r / \n，曾经因为按行切分
	// 导致整段输出憋到 EOF，界面进度条一直停在 0%、结束时直接跳到 100%。
	// 这里要求进度在下载过程中就陆续到达，而不是结束时才来一次。
	if frames < 2 {
		t.Fatalf("进度只在结束时到达 %d 次，说明读输出又退化了（进度会停在 0%%）", frames)
	}
	if firstAt > total/2 {
		t.Fatalf("首帧进度来得太晚: %s / 总耗时 %s，下载过程中界面不会有实时进度", firstAt, total)
	}
}
