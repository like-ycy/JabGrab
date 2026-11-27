package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// 本地合成 HLS：使用真实 curl、dl-re 和 ffmpeg 验证下载及混流，无需外站。
func TestCurlTransportE2E(t *testing.T) {
	if os.Getenv("CURL_DLR_E2E") == "" {
		t.Skip("设置 CURL_DLR_E2E=1 才运行工具集成测试")
	}
	ffmpeg := FindFFmpeg()
	if ffmpeg == "" {
		t.Fatal("缺少 ffmpeg")
	}
	name := "N_m3u8DL-RE"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin, err := filepath.Abs(filepath.Join("../../../third_party/dlre", runtime.GOOS+"-"+runtime.GOARCH, name))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "source")
	if err := os.Mkdir(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	generate := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=44100",
		"-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(fixture, "segment%d.ts"), filepath.Join(fixture, "video.m3u8"))
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("生成 HLS: %v %s", err, output)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 拒绝 dl-re 的默认网络请求，只让带正确来源信息的 curl 访问。
		if r.Header.Get("Cache-Control") == "no-cache" || r.Header.Get("User-Agent") != "browser-e2e" || r.Header.Get("Referer") != "https://missav.ws/cn/test" {
			http.Error(w, "rejected client", 403)
			return
		}
		http.FileServer(http.Dir(fixture)).ServeHTTP(w, r)
	}))
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	opts := Options{Bin: bin, SaveDir: filepath.Join(dir, "output"), SaveName: "result", Threads: 2, RetryCount: 1,
		TimeoutSec: 10, MuxToMP4: true, FFmpegPath: ffmpeg, CurlTransport: true,
		Headers: map[string]string{"user-agent": "browser-e2e", "referer": "https://missav.ws/cn/test"}}
	if err := opts.Run(ctx, origin.URL+"/video.m3u8", nil, func(line string) { t.Log(line) }); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	output, err := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_type,width,height", "-of", "json", filepath.Join(opts.SaveDir, "result.mp4")).Output()
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Streams []struct {
			Type          string `json:"codec_type"`
			Width, Height int
		}
	}
	if err := json.Unmarshal(output, &metadata); err != nil {
		t.Fatal(err)
	}
	video, audio := false, false
	for _, stream := range metadata.Streams {
		video = video || (stream.Type == "video" && stream.Width == 1280 && stream.Height == 720)
		audio = audio || stream.Type == "audio"
	}
	if !video || !audio {
		t.Fatal(fmt.Sprintf("混流缺少预期音视频: %s", output))
	}
}

// 真实下载端到端探针，默认跳过：
//
//	DLR_E2E=1 go test ./internal/downloader -run TestE2EProbe -v
func TestE2EProbe(t *testing.T) {
	if os.Getenv("DLR_E2E") == "" {
		t.Skip("设置 DLR_E2E=1 才运行真实下载")
	}

	opts := Options{
		Bin:        "../../third_party/dlre/" + runtime.GOOS + "-" + runtime.GOARCH + "/N_m3u8DL-RE",
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
