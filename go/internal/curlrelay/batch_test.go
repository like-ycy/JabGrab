package curlrelay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchReusesConnections(t *testing.T) {
	var connects atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		if r.Header.Get("X-Test") != `quote" slash\ percent%` {
			t.Errorf("header changed: %q", r.Header.Get("X-Test"))
		}
		fmt.Fprint(w, r.URL.RequestURI())
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connects.Add(1)
		}
	}
	origin.Start()
	defer origin.Close()
	bin, err := FindCurl()
	if err != nil {
		t.Fatal(err)
	}
	// 同一组四个请求，以旧方式各启动一个 curl，直接测量连接数基线。
	for i := 0; i < 4; i++ {
		cmd := exec.Command(bin, "-q", "-sS", "--http1.1", "--noproxy", "*", "-H", `X-Test: quote" slash\ percent%`, origin.URL+fmt.Sprintf("/%d", i))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("baseline: %v %s", err, output)
		}
	}
	baseline := connects.Swap(0)
	if baseline != 4 {
		t.Fatalf("baseline connections=%d", baseline)
	}
	tr, err := newTransport(context.Background(), bin, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	jobs := make([]*transfer, 4)
	for i := range jobs {
		jobs[i] = &transfer{ctx: context.Background(), target: origin.URL + fmt.Sprintf("/%d?q=a%%2Bb", i), method: "GET", headers: http.Header{"X-Test": {`quote" slash\ percent%`}}, timeout: time.Second, result: make(chan transferResult, 1)}
	}
	// 直接提交一批，避免计时器/调度影响连接复用断言。
	tr.runBatch(jobs, 2)
	for i, job := range jobs {
		result := <-job.result
		if result.err != nil {
			t.Fatal(result.err)
		}
		data, err := io.ReadAll(result.response.body)
		result.response.close()
		if err != nil || string(data) != fmt.Sprintf("/%d?q=a%%2Bb", i) {
			t.Fatalf("response %d: %s %v", i, data, err)
		}
	}
	stats := tr.stats()
	if stats.Requests != 4 || stats.Processes != 1 || connects.Load() > 2 || stats.Connections > 2 {
		t.Fatalf("reuse failed: stats=%+v connections=%d", stats, connects.Load())
	}
	t.Logf("4 requests: processes 4 -> %d, TCP connections %d -> %d", stats.Processes, baseline, connects.Load())
	entries, err := os.ReadDir(tr.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files retained: %v %v", entries, err)
	}
}

func TestBatchIsolatesFailedTransfer(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.Header().Set("Content-Length", "1000")
			fmt.Fprint(w, "short")
			return
		}
		fmt.Fprint(w, "complete")
	}))
	defer origin.Close()
	bin, _ := FindCurl()
	tr, err := newTransport(context.Background(), bin, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	jobs := []*transfer{}
	for _, path := range []string{"/bad", "/good"} {
		jobs = append(jobs, &transfer{ctx: context.Background(), target: origin.URL + path, method: "GET", timeout: time.Second, result: make(chan transferResult, 1)})
	}
	tr.runBatch(jobs, 1)
	bad, good := <-jobs[0].result, <-jobs[1].result
	if bad.err == nil || bad.response != nil {
		t.Fatalf("truncated transfer accepted: %+v", bad)
	}
	if good.err != nil {
		t.Fatal(good.err)
	}
	data, err := io.ReadAll(good.response.body)
	good.response.close()
	if err != nil || string(data) != "complete" {
		t.Fatalf("good transfer lost: %q %v", data, err)
	}
}

func TestBatchCancellationAndConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	started := make(chan struct{}, 32)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer origin.Close()
	bin, _ := FindCurl()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr, err := newTransport(ctx, bin, 3)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			resp, err := tr.fetch(ctx, origin.URL, "GET", nil, 10*time.Second)
			if resp != nil {
				resp.close()
			}
			if err == nil {
				t.Error("canceled request succeeded")
			}
		})
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no request")
	}
	cancel()
	done := make(chan struct{})
	go func() { tr.close(); wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch cancellation hung")
	}
	if peak.Load() > 3 {
		t.Fatalf("configured concurrency exceeded: %d", peak.Load())
	}
	if _, err := os.Stat(tr.dir); !os.IsNotExist(err) {
		t.Fatalf("temporary directory retained: %v", err)
	}
}

func TestBatchCanceledSiblingDoesNotCancelGoodTransfer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cancel" {
			close(started)
			<-release
		}
		fmt.Fprint(w, r.URL.Path)
	}))
	defer origin.Close()
	bin, _ := FindCurl()
	tr, err := newTransport(context.Background(), bin, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := []*transfer{
		{ctx: ctx, target: origin.URL + "/cancel", method: "GET", timeout: time.Second, result: make(chan transferResult)},
		{ctx: context.Background(), target: origin.URL + "/good", method: "GET", timeout: time.Second, result: make(chan transferResult, 1)},
	}
	done := make(chan struct{})
	go func() { tr.runBatch(jobs, 2); close(done) }()
	<-started
	cancel()
	close(release)
	<-done
	result := <-jobs[1].result
	if result.err != nil {
		t.Fatal(result.err)
	}
	data, err := io.ReadAll(result.response.body)
	result.response.close()
	if err != nil || string(data) != "/good" {
		t.Fatalf("canceled sibling broke good response: %q %v", data, err)
	}
}

func TestReadResultUsesFinalHeadersAfterProxyConnect(t *testing.T) {
	dir := t.TempDir()
	data := "HTTP/1.1 200 Connection established\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </x>\r\n\r\nHTTP/1.1 206 Partial Content\r\nContent-Range: bytes 0-2/100\r\n\r\n"
	if err := os.WriteFile(filepath.Join(dir, "0.head"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0.body"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	files := &batchFiles{dir: dir}
	files.refs.Store(1)
	result, err := readResult(files, 0, 206)
	if err != nil {
		t.Fatal(err)
	}
	defer result.close()
	defer files.release()
	if !strings.HasPrefix(result.header.Get("Content-Range"), "bytes 0-2") {
		t.Fatal(result.header)
	}
}

func TestBatchDeliversBeforeSlowSibling(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, r.URL.Path)
	}))
	defer origin.Close()
	defer finish()
	bin, _ := FindCurl()
	tr, err := newTransport(context.Background(), bin, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	jobs := []*transfer{}
	for _, path := range []string{"/slow", "/fast"} {
		jobs = append(jobs, &transfer{ctx: context.Background(), target: origin.URL + path,
			method: "GET", timeout: 5 * time.Second, result: make(chan transferResult, 1)})
	}
	done := make(chan struct{})
	go func() { tr.runBatch(jobs, 2); close(done) }()
	select {
	case fast := <-jobs[1].result:
		if fast.err != nil {
			t.Fatal(fast.err)
		}
		body, err := io.ReadAll(fast.response.body)
		fast.response.close()
		if err != nil || string(body) != "/fast" {
			t.Fatalf("fast response: %q %v", body, err)
		}
	case <-time.After(3 * time.Second):
		t.Error("fast request waited for slow sibling")
	}
	finish()
	<-done
	slow := <-jobs[0].result
	if slow.err != nil {
		t.Fatal(slow.err)
	}
	slow.response.close()
}
