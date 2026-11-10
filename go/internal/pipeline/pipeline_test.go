package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"jabgrab/internal/config"
)

func TestReadURLs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "urls.txt")
	content := `# 注释行
https://jable.tv/videos/abc-123/
https://jable.tv/videos/abc-123/
   
https://missav.com/en/xyz-456
不合法的一行
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadURLs(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://jable.tv/videos/abc-123/",
		"https://missav.com/en/xyz-456",
		"不合法的一行",
	}
	if len(got) != len(want) {
		t.Fatalf("数量不符 got=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符 got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestReadURLsMissing(t *testing.T) {
	if _, err := ReadURLs(""); err == nil {
		t.Fatal("空路径应报错")
	}
	if _, err := ReadURLs(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}

func TestBuildItems(t *testing.T) {
	cfg := config.Default()
	cfg.Sites = map[string]config.Site{
		"jable":  {Enabled: true, Keyword: "jable"},
		"missav": {Enabled: false, Keyword: "missav"},
	}

	urls := []string{
		"https://jable.tv/videos/a/",
		"https://missav.com/en/b",
		"https://other.com/c",
	}
	items := buildItems(cfg, urls)

	if items[0].Site != "jable" || items[0].Status != StatusPending {
		t.Fatalf("jable 应入队: %+v", items[0])
	}
	// 站点被禁用 → 忽略
	if items[1].Status != StatusIgnored {
		t.Fatalf("禁用的 missav 应被忽略: %+v", items[1])
	}
	if items[2].Status != StatusIgnored {
		t.Fatalf("不匹配的 URL 应被忽略: %+v", items[2])
	}
}

func TestExistingVideo(t *testing.T) {
	dir := t.TempDir()
	if got := existingVideo(dir, "ABC-123"); got != "" {
		t.Fatalf("空目录不应命中: %q", got)
	}

	// 空文件不算已下载（避免上次中断留下的空壳文件被误判）
	empty := filepath.Join(dir, "ABC-123.mp4")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := existingVideo(dir, "ABC-123"); got != "" {
		t.Fatalf("0 字节文件不应命中: %q", got)
	}

	if err := os.WriteFile(empty, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := existingVideo(dir, "ABC-123"); got != empty {
		t.Fatalf("应命中已有文件 got=%q", got)
	}

	// 不同名字不能误命中
	if got := existingVideo(dir, "ABC-123-C"); got != "" {
		t.Fatalf("不应命中其他名字: %q", got)
	}
}

func TestRunnerSnapshotInitial(t *testing.T) {
	r := New(config.Default(), Hooks{})
	st := r.Snapshot()
	if st.Running || len(st.Items) != 0 {
		t.Fatalf("初始状态应为空: %+v", st)
	}
	if r.Running() {
		t.Fatal("初始不应在运行")
	}
}

func TestRunRejectsEmptyTxtPath(t *testing.T) {
	cfg := config.Default()
	cfg.TxtPath = ""
	r := New(cfg, Hooks{})
	if err := r.Run(t.Context()); err == nil {
		t.Fatal("没有 txt 路径时应报错")
	}
}
