package curlrelay

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startTestRelay(t *testing.T, raw string) *Relay {
	t.Helper()
	r, err := Start(context.Background(), Options{URL: raw, Concurrency: 2, Timeout: 5 * time.Second,
		Headers: map[string]string{"user-agent": "test-browser", "referer": "https://missav.ws/cn/test", "origin": "https://missav.ws"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func getBody(t *testing.T, raw string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func mediaLines(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func TestRelayResolvesRedirectsKeysMapsAndRanges(t *testing.T) {
	var fetched atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		if r.Header.Get("User-Agent") != "test-browser" || r.Header.Get("Origin") != "https://missav.ws" || r.Header.Get("Referer") != "https://missav.ws/cn/test" {
			t.Errorf("来源请求头丢失: %v", r.Header)
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/start.m3u8":
			http.Redirect(w, r, "/media/master.m3u8", 302)
		case "/media/master.m3u8":
			fmt.Fprint(w, "\xef\xbb\xbf#EXTM3U\r\n#EXT-X-STREAM-INF:BANDWIDTH=100\r\nvideo.m3u8?token=a%2Bb\r\n")
		case "/media/video.m3u8":
			if r.URL.RawQuery != "token=a%2Bb" {
				t.Errorf("查询参数被改写: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key.bin?k=1\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/key.bin":
			fmt.Fprint(w, "0123456789abcdef")
		case "/media/init.mp4":
			fmt.Fprint(w, "initialization")
		case "/media/segment.ts":
			w.Header().Set("Accept-Ranges", "bytes")
			if r.Header.Get("Range") == "bytes=2-4" {
				w.Header().Set("Content-Range", "bytes 2-4/10")
				w.WriteHeader(206)
				fmt.Fprint(w, "234")
			} else {
				fmt.Fprint(w, "0123456789")
			}
		default:
			t.Errorf("错误的相对路径: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer origin.Close()
	r := startTestRelay(t, origin.URL+"/start.m3u8")
	resp, master := getBody(t, r.URL())
	if resp.StatusCode != 200 || !bytes.HasPrefix(master, []byte("#EXTM3U")) {
		t.Fatalf("主清单失败: %d %s (%v)", resp.StatusCode, master, r.Err())
	}
	_, media := getBody(t, mediaLines(master)[0])
	refs := uriAttribute.FindAllSubmatch(media, -1)
	if len(refs) != 2 {
		t.Fatalf("密钥和初始化清单未保留: %s", media)
	}
	for i, want := range []string{"0123456789abcdef", "initialization"} {
		_, data := getBody(t, string(refs[i][1]))
		if string(data) != want {
			t.Fatalf("资源 %d: %q", i, data)
		}
	}
	segment := mediaLines(media)[0]
	request, _ := http.NewRequest(http.MethodGet, segment, nil)
	request.Header.Set("Range", "bytes=2-4")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 206 || string(body) != "234" || response.Header.Get("Content-Range") != "bytes 2-4/10" {
		t.Fatalf("范围请求失败: %v %d %q %v", err, response.StatusCode, body, response.Header)
	}
	response, err = http.Head(segment)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength != 10 || fetched.Load() != 7 {
		t.Fatalf("HEAD/请求计数异常: %d %d %d", response.StatusCode, response.ContentLength, fetched.Load())
	}
}

func TestRelayDecodesCompressedChunkedManifest(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.(http.Flusher).Flush()
		z := gzip.NewWriter(w)
		fmt.Fprint(z, "#EXTM3U\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n")
		z.Close()
	}))
	defer origin.Close()
	r := startTestRelay(t, origin.URL+"/list.m3u8")
	resp, body := getBody(t, r.URL())
	if resp.StatusCode != 200 || !bytes.HasPrefix(body, []byte("#EXTM3U")) || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("压缩清单解码失败: %d %q %v", resp.StatusCode, body, r.Err())
	}
}

func TestRelayRejectsForeignTargetsAndInvalidManifests(t *testing.T) {
	for _, body := range []string{"<html>denied</html>", "#EXTM3U\nhttps://foreign.invalid/a.ts\n", "#EXTM3U\nfile:///etc/passwd\n", "#EXTM3U\n{$secret}/a.ts\n"} {
		t.Run(body, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer origin.Close()
			r := startTestRelay(t, origin.URL+"/list.m3u8")
			resp, _ := getBody(t, r.URL())
			if resp.StatusCode != 502 || r.Err() == nil {
				t.Fatalf("无效源被当成成功: %d %v", resp.StatusCode, r.Err())
			}
		})
	}
}

func TestRelayPreservesSourceErrorAndRejectsUnknownRoute(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer origin.Close()
	r := startTestRelay(t, origin.URL+"/list.m3u8")
	resp, _ := getBody(t, r.URL())
	if resp.StatusCode != 502 || !strings.Contains(r.Err().Error(), "403") {
		t.Fatalf("源站错误丢失: %d %v", resp.StatusCode, r.Err())
	}
	for _, path := range []string{r.URL() + "?url=http://foreign.invalid/", r.URL() + "/wrong"} {
		resp, _ := getBody(t, path)
		if resp.StatusCode != 404 {
			t.Fatalf("未知路由未拒绝: %d", resp.StatusCode)
		}
	}
}

func TestCloseCancelsActiveCurl(t *testing.T) {
	started, released := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(released)
	}))
	defer origin.Close()
	r := startTestRelay(t, origin.URL+"/list.m3u8")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, _ := http.Get(r.URL())
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("curl 未发起请求")
	}
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	for _, ch := range []chan struct{}{closed, released, done} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("停止后 curl 或请求未退出")
		}
	}
}

func TestPartialSegmentIsNotSuccessful(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n")
			return
		}
		w.Header().Set("Content-Length", "100000")
		w.Write(bytes.Repeat([]byte("a"), 10000))
	}))
	defer origin.Close()
	r := startTestRelay(t, origin.URL+"/list.m3u8")
	_, body := getBody(t, r.URL())
	resp, err := http.Get(mediaLines(body)[0])
	if err == nil {
		status := resp.StatusCode
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if status == http.StatusBadGateway && r.Err() != nil {
			return // curl 在暂存阶段已发现截断，尚未向下载器交付任何分片字节。
		}
	}
	if err == nil {
		t.Fatal("截断的分片被报告为成功")
	}
}

func TestSourceValidation(t *testing.T) {
	for _, raw := range []string{"file:///tmp/x", "https://user:pass@example.com/x"} {
		if r, err := Start(context.Background(), Options{URL: raw}); err == nil {
			r.Close()
			t.Fatalf("接受了无效源 %s", raw)
		}
	}
	r := startTestRelay(t, "https://c.surrit.com/abc/list.m3u8")
	for _, raw := range []string{"http://127.0.0.1/x", "https://c.surrit.com.evil.test/x", "https://surrit.com:1234/x"} {
		u, _ := url.Parse(raw)
		if _, err := r.register(u); err == nil {
			t.Fatalf("允许跨源 %s", raw)
		}
	}
}
