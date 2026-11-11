package pipeline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jabgrab/internal/config"
)

func TestReadURLs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "urls.txt")
	content := `# 注释行
https://jable.tv/videos/abc-123/
https://jable.tv/videos/abc-123/
https://jable.tv/videos/abc-123
https://jable.tv/videos/abc-123#comments
   
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
		"https://jable.tv/videos/d/",
	}
	items := buildItems(cfg, urls)

	if len(items) != 2 {
		t.Fatalf("禁用站点和不匹配的 URL 不应进入任务列表: %+v", items)
	}
	if items[0].Site != "jable" || items[0].Status != StatusPending {
		t.Fatalf("jable 应入队: %+v", items[0])
	}
	if items[0].URL != urls[0] || items[1].URL != urls[3] {
		t.Fatalf("过滤后应保持可处理 URL 的原顺序: %+v", items)
	}
	if got := buildItems(cfg, urls[1:3]); len(got) != 0 {
		t.Fatalf("全部忽略时应为空列表: %+v", got)
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

// 目标目录优先，其次回落到保存目录根与一级子目录（兼容改动前下好的 / 演员没解析出来的）。
func TestFindExisting(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, "三上悠亜")
	actorPath := filepath.Join(root, "釈アリス", "ABC-123.mp4")
	rootPath := filepath.Join(root, "ABC-123.mp4")

	if got := findExisting(root, filepath.Join(root, "釈アリス"), "ABC-123"); got != "" {
		t.Fatalf("都没有时不应命中: %q", got)
	}

	if err := os.MkdirAll(filepath.Dir(actorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(actorPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findExisting(root, filepath.Join(root, "釈アリス"), "ABC-123"); got != actorPath {
		t.Fatalf("应命中目标演员目录 got=%q want=%q", got, actorPath)
	}
	// 本次没解析出演员（actors 为空 → 目标是根目录）时，也要能发现演员目录里的成品，
	// 否则同一部片子会被重复下载。
	if got := findExisting(root, root, "ABC-123"); got != actorPath {
		t.Fatalf("无演员时应回落到子目录 got=%q want=%q", got, actorPath)
	}
	// 解析出了别的演员，但片子已在另一个演员目录里
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findExisting(root, other, "ABC-123"); got != actorPath {
		t.Fatalf("应扫到其他演员目录 got=%q", got)
	}

	if err := os.WriteFile(rootPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findExisting(root, root, "ABC-123"); got != rootPath {
		t.Fatalf("根目录优先于子目录 got=%q", got)
	}
	// 不同名字不能误命中
	if got := findExisting(root, root, "ABC-123-C"); got != "" {
		t.Fatalf("不应命中其他名字: %q", got)
	}
}

func TestURLKey(t *testing.T) {
	cases := map[string]string{
		"https://jable.tv/videos/abc-123/":  "https://jable.tv/videos/abc-123",
		"https://jable.tv/videos/abc-123":   "https://jable.tv/videos/abc-123",
		"https://jable.tv/videos/abc-123#c": "https://jable.tv/videos/abc-123",
		// query 保留：不同参数可能是不同资源
		"https://jable.tv/videos/a?p=2": "https://jable.tv/videos/a?p=2",
	}
	for in, want := range cases {
		if got := urlKey(in); got != want {
			t.Fatalf("urlKey(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPlainName(t *testing.T) {
	if got, ok := plainName("ABC-123-C"); !ok || got != "ABC-123" {
		t.Fatalf("应识别中文字幕版 got=%q ok=%v", got, ok)
	}
	// 普通版不应被当成字幕版
	if got, ok := plainName("ABC-123"); ok || got != "" {
		t.Fatalf("普通版不应命中 got=%q ok=%v", got, ok)
	}
	// 只有后缀没有番号时不能返回空基础名，否则会误删整个目录下的同名文件
	if _, ok := plainName("-C"); ok {
		t.Fatal("空基础名不应返回 ok")
	}
	// 番号本身以 -C 结尾时只去一层
	if got, ok := plainName("ABC-123-C-C"); !ok || got != "ABC-123-C" {
		t.Fatalf("应只去掉一层后缀 got=%q ok=%v", got, ok)
	}
}

// 中文字幕版下完后，同名的非中文字幕旧档要清掉：
// 目标演员目录、保存目录根、其它演员目录里的都算。
func TestRemovePlainVideo(t *testing.T) {
	root := t.TempDir()
	actor := filepath.Join(root, "釈アリス")
	other := filepath.Join(root, "三上悠亜")
	for _, d := range []string{actor, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	write := func(p string) {
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 普通版散落在三处：目标目录、根目录、别的演员目录
	inActor := filepath.Join(actor, "ABC-123.mp4")
	inRoot := filepath.Join(root, "ABC-123.mkv")
	inOther := filepath.Join(other, "ABC-123.mp4")
	for _, p := range []string{inActor, inRoot, inOther} {
		write(p)
	}
	// 不是同一部片子的文件，绝不能动
	keep := filepath.Join(actor, "ABC-124.mp4")
	write(keep)

	// 还没有中文字幕版时一个都不能删：删了就是把这部片子删没了
	if removed, err := removePlainVideo(root, actor, "ABC-123"); err != nil || len(removed) != 0 {
		t.Fatalf("没有 -C 版时不应删除 removed=%v err=%v", removed, err)
	}
	for _, p := range []string{inActor, inRoot, inOther} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("文件应保留 %s", p)
		}
	}

	// 中文字幕版就位后，三处普通版清空
	sub := filepath.Join(actor, "ABC-123-C.mp4")
	write(sub)

	removed, err := removePlainVideo(root, actor, "ABC-123")
	if err != nil {
		t.Fatalf("删除不应报错: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("应删掉 3 个普通版 got=%v", removed)
	}
	for _, p := range []string{inActor, inRoot, inOther} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("应已删除 %s", p)
		}
	}
	for _, p := range []string{keep, sub} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("不应删除 %s: %v", p, err)
		}
	}
}

// 下载器报成功但只留了个残件时，绝不能删掉完整的普通版。
func TestDropPlainVideoKeepsPlainWhenSubtitleIncomplete(t *testing.T) {
	root := t.TempDir()
	actor := filepath.Join(root, "釈アリス")
	if err := os.MkdirAll(actor, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(actor, "ABC-123.mp4")
	sub := filepath.Join(actor, "ABC-123-C.mp4")
	if err := os.WriteFile(plain, bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	// 明显是碎片：不足普通版的 1/2
	if err := os.WriteFile(sub, []byte("frag"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs []string
	r := New(config.Default(), Hooks{OnLog: func(level, text string) {
		logs = append(logs, level+" "+text)
	}})
	r.dropPlainVideo(1, 1, root, actor, "ABC-123")

	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("残件场景下普通版应保留: %v", err)
	}
	warned := false
	for _, l := range logs {
		if strings.HasPrefix(l, "warn ") && strings.Contains(l, "未下载完整") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("应给出未完整告警 got=%v", logs)
	}

	// 换成正常大小的成品后才会真的清理
	if err := os.WriteFile(sub, bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	r.dropPlainVideo(1, 1, root, actor, "ABC-123")
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatal("成品完整时应删除普通版")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("中文字幕版应保留: %v", err)
	}
}

// 下载前判断：普通版链接遇到已有中文字幕版要跳过；
// 中文字幕版链接遇到只有普通版时仍要下载（否则永远升不上去）。
func TestNameCandidates(t *testing.T) {
	plain := nameCandidates("ABC-123")
	if len(plain) != 2 || plain[0] != "ABC-123" || plain[1] != "ABC-123-C" {
		t.Fatalf("普通版应同时接受两个版本 got=%v", plain)
	}
	sub := nameCandidates("ABC-123-C")
	if len(sub) != 1 || sub[0] != "ABC-123-C" {
		t.Fatalf("中文字幕版只认自己 got=%v", sub)
	}
}

func TestFindExistingVersion(t *testing.T) {
	root := t.TempDir()
	actor := filepath.Join(root, "釈アリス")
	if err := os.MkdirAll(actor, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(actor, "ABC-123.mp4")
	sub := filepath.Join(actor, "ABC-123-C.mp4")

	// 本地只有普通版：普通版链接跳过，中文字幕版链接照下
	if err := os.WriteFile(plain, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findExistingVersion(root, actor, "ABC-123"); got != plain {
		t.Fatalf("普通版应命中 got=%q", got)
	}
	if got := findExistingVersion(root, actor, "ABC-123-C"); got != "" {
		t.Fatalf("只有普通版时中文字幕版不应跳过 got=%q", got)
	}

	// 本地两份都在：两种链接都跳过
	if err := os.WriteFile(sub, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findExistingVersion(root, actor, "ABC-123-C"); got != sub {
		t.Fatalf("中文字幕版应命中 got=%q", got)
	}
	if got := findExistingVersion(root, actor, "ABC-123"); got != plain {
		t.Fatalf("普通版应命中已有普通版 got=%q", got)
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
