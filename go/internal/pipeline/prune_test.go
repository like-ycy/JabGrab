package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTxt(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readTxt(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPruneURLsKeepsUnfinishedLines(t *testing.T) {
	const content = `# 待下载清单
https://jable.tv/videos/abc-123/

https://jable.tv/videos/def-456
https://missav.com/en/xyz-789
`
	p := writeTxt(t, content)

	res, err := PruneURLs(p, []string{"https://jable.tv/videos/abc-123"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Kept != 2 {
		t.Fatalf("统计不符 removed=%d kept=%d", res.Removed, res.Kept)
	}
	// 注释、空行、失败的两条都要原样留下，顺序不变。
	want := `# 待下载清单

https://jable.tv/videos/def-456
https://missav.com/en/xyz-789
`
	if got := readTxt(t, p); got != want {
		t.Fatalf("文件内容不符\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestPruneURLsIgnoresTrailingSlashAndFragment(t *testing.T) {
	p := writeTxt(t, "https://jable.tv/videos/abc-123/#comments\nhttps://jable.tv/videos/def-456\n")

	res, err := PruneURLs(p, []string{"https://jable.tv/videos/abc-123"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("应删 1 条，实际 %d", res.Removed)
	}
	if got := readTxt(t, p); got != "https://jable.tv/videos/def-456\n" {
		t.Fatalf("剩下内容不符: %q", got)
	}
}

func TestPruneURLsKeepsCRLF(t *testing.T) {
	p := writeTxt(t, "https://a.com/1\r\nhttps://a.com/2\r\n")

	if _, err := PruneURLs(p, []string{"https://a.com/1"}); err != nil {
		t.Fatal(err)
	}
	if got := readTxt(t, p); got != "https://a.com/2\r\n" {
		t.Fatalf("CRLF 被改写: %q", got)
	}
}

func TestPruneURLsKeepsBOM(t *testing.T) {
	p := writeTxt(t, "\ufeffhttps://a.com/1\nhttps://a.com/2\n")

	res, err := PruneURLs(p, []string{"https://a.com/1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("BOM 挡住的匹配，应仍能删掉 1 条，实际 %d", res.Removed)
	}
	// 删掉首行后 BOM 随之消失，剩下的内容不能带任何残留字符。
	if got := readTxt(t, p); got != "https://a.com/2\n" {
		t.Fatalf("剩下内容不符: %q", got)
	}
}

func TestPruneURLsNoMatchLeavesFileAlone(t *testing.T) {
	p := writeTxt(t, "https://a.com/1\n")

	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := PruneURLs(p, []string{"https://a.com/2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || res.Kept != 1 {
		t.Fatalf("统计不符 removed=%d kept=%d", res.Removed, res.Kept)
	}
	// 一条都没命中就不要碰文件：临时文件 + 改名会改掉修改时间。
	st2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(st2.ModTime()) {
		t.Fatal("没命中任何行时不应改写文件")
	}
}

func TestPruneURLsEmptyList(t *testing.T) {
	p := writeTxt(t, "https://a.com/1\n")

	res, err := PruneURLs(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || res.Kept != 1 {
		t.Fatalf("统计不符 removed=%d kept=%d", res.Removed, res.Kept)
	}
}

func TestPruneURLsPreservesMode(t *testing.T) {
	p := writeTxt(t, "https://a.com/1\nhttps://a.com/2\n")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneURLs(p, []string{"https://a.com/1"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("权限被改成 %o", st.Mode().Perm())
	}
}

func TestPruneURLsRemovesAll(t *testing.T) {
	p := writeTxt(t, "https://a.com/1\nhttps://a.com/2\n")

	res, err := PruneURLs(p, []string{"https://a.com/1", "https://a.com/2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 2 || res.Kept != 0 {
		t.Fatalf("统计不符 removed=%d kept=%d", res.Removed, res.Kept)
	}
	if got := readTxt(t, p); strings.TrimSpace(got) != "" {
		t.Fatalf("应清空，实际 %q", got)
	}
}

func TestPruneURLsMissingFile(t *testing.T) {
	if _, err := PruneURLs(filepath.Join(t.TempDir(), "nope.txt"), []string{"https://a.com/1"}); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, err := PruneURLs("", []string{"https://a.com/1"}); err == nil {
		t.Fatal("空路径应报错")
	}
}

func TestDownloaded(t *testing.T) {
	for _, st := range []Status{StatusDone, StatusSkipped} {
		if !Downloaded(st) {
			t.Fatalf("%s 应算已下载", st)
		}
	}
	for _, st := range []Status{StatusPending, StatusScraping, StatusDownloading, StatusFailed, StatusCancelled} {
		if Downloaded(st) {
			t.Fatalf("%s 不应算已下载", st)
		}
	}
}
