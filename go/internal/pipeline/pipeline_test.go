package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jabgrab/internal/config"
	"jabgrab/internal/scraper"
)

// stubDuration 把时长探测换成桩：清理用例不该依赖本机是否装了 ffmpeg。
// byPath 指定具体文件的时长，missing 里的文件返回错误。
func stubDuration(t *testing.T, byPath map[string]time.Duration, missing ...string) {
	t.Helper()
	bad := make(map[string]bool, len(missing))
	for _, p := range missing {
		bad[p] = true
	}
	old := mediaDurationFn
	t.Cleanup(func() { mediaDurationFn = old })
	mediaDurationFn = func(ffmpeg, file string) (time.Duration, error) {
		if bad[file] {
			return 0, errors.New("读不到时长")
		}
		if d, ok := byPath[file]; ok {
			return d, nil
		}
		return 0, errors.New("未预置时长: " + file)
	}
}

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

// 清理这一步无论结局如何都要在运行日志里留痕：日志窗口里能看出它确实跑过。
// 「没删东西」和「没执行」在界面上必须能区分开。
func TestDropPlainVideoLogsEveryOutcome(t *testing.T) {
	root := t.TempDir()
	actor := filepath.Join(root, "釈アリス")
	if err := os.MkdirAll(actor, 0o755); err != nil {
		t.Fatal(err)
	}
	newRunner := func() (*Runner, *[]string) {
		var logs []string
		r := New(config.Default(), Hooks{OnLog: func(level, text string) {
			logs = append(logs, level+" "+text)
		}})
		return r, &logs
	}

	// 从没下过普通版：也要有一行 info 交代「无需清理」
	r, logs := newRunner()
	r.dropPlainVideo(1, 2, root, actor, "ABC-123", "", "")
	if !hasLog(*logs, "info", "没有同名的非中文字幕旧档需要清理") {
		t.Fatalf("无旧档时应写一行日志 got=%v", *logs)
	}

	// 普通版 + 等长的中文字幕版：删除动作本身也要进日志，并带上被删的路径
	plain := filepath.Join(actor, "ABC-123.mp4")
	sub := filepath.Join(actor, "ABC-123-C.mp4")
	for _, p := range []string{plain, sub} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stubDuration(t, map[string]time.Duration{
		plain: 20 * time.Minute,
		sub:   20 * time.Minute,
	})
	r, logs = newRunner()
	r.dropPlainVideo(1, 2, root, actor, "ABC-123", sub, "/usr/bin/ffmpeg")
	if !hasLog(*logs, "info", "已删除同名的非中文字幕旧档："+plain) {
		t.Fatalf("删除后应写一行带路径的日志 got=%v", *logs)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatal("普通版应已删除")
	}
}

func hasLog(logs []string, level, substr string) bool {
	for _, l := range logs {
		if strings.HasPrefix(l, level+" ") && strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// 「已存在」日志里要带体积：正片和残件的差别一眼可见，文档也这么承诺。
func TestDescribeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ABC-123.mp4")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fileSize(p); got != "2.00KB" {
		t.Fatalf("fileSize=%q want 2.00KB", got)
	}
	if got := describeFile(p); got != p+"（2.00KB）" {
		t.Fatalf("describeFile=%q", got)
	}
	// 空文件与不存在的文件都不带大小，退回纯路径
	empty := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := describeFile(empty); got != empty {
		t.Fatalf("空文件不应带大小 got=%q", got)
	}
	missing := filepath.Join(dir, "missing.mp4")
	if got := describeFile(missing); got != missing {
		t.Fatalf("不存在的文件应退回纯路径 got=%q", got)
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

// 中文字幕版下完后，同名的非中文字幕旧档要清掉：目标演员目录、保存目录根、
// 其它演员目录里的都算；每个候选都要通过校验才删，不同名的文件一个都不能动。
func TestDropPlainVideoDeletesAllVerifiedCandidates(t *testing.T) {
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

	inActor := filepath.Join(actor, "ABC-123.mp4")
	inRoot := filepath.Join(root, "ABC-123.mkv")
	inOther := filepath.Join(other, "ABC-123.mp4")
	for _, p := range []string{inActor, inRoot, inOther} {
		write(p)
	}
	// 不是同一部片子的文件，绝不能动
	keep := filepath.Join(actor, "ABC-124.mp4")
	write(keep)

	cands := plainCandidates(root, actor, "ABC-123")
	if len(cands) != 3 {
		t.Fatalf("三个目录的同名普通版都应成为候选 got=%v", cands)
	}
	// 还没有中文字幕版时一个都不能删：删了就是把这部片子删没了
	if len(plainCandidates(root, actor, "ABC-999")) != 0 {
		t.Fatal("不存在的名字不应有候选")
	}

	sub := filepath.Join(actor, "ABC-123-C.mp4")
	write(sub)
	equal := map[string]time.Duration{}
	for _, p := range append([]string{sub}, cands...) {
		equal[p] = 24 * time.Minute
	}
	stubDuration(t, equal)

	var logs []string
	r := New(config.Default(), Hooks{OnLog: func(level, text string) {
		logs = append(logs, level+" "+text)
	}})
	r.dropPlainVideo(1, 1, root, actor, "ABC-123", sub, "/usr/bin/ffmpeg")

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
	if !hasLog(logs, "info", "已删除同名的非中文字幕旧档："+inRoot) {
		t.Fatalf("每个被删的路径都要有日志 got=%v", logs)
	}
}

// 时长是唯一能证明「新版可替代旧档」的证据，任何一步拿不到就保留旧文件。
func TestDropPlainVideoKeepsPlainWhenNotVerifiable(t *testing.T) {
	root := t.TempDir()
	actor := filepath.Join(root, "釈アリス")
	if err := os.MkdirAll(actor, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(actor, "ABC-123.mp4")
	sub := filepath.Join(actor, "ABC-123-C.mp4")
	for _, p := range []string{plain, sub} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	newRunner := func() (*Runner, *[]string) {
		var logs []string
		r := New(config.Default(), Hooks{OnLog: func(level, text string) {
			logs = append(logs, level+" "+text)
		}})
		return r, &logs
	}

	// 1) 新版时长读不到：整批保留
	stubDuration(t, map[string]time.Duration{plain: 24 * time.Minute}, sub)
	r, logs := newRunner()
	r.dropPlainVideo(1, 1, root, actor, "ABC-123", sub, "/usr/bin/ffmpeg")
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("读不到新版时长时应保留旧档: %v", err)
	}
	if !hasLog(*logs, "warn", "读不到中文字幕版时长") {
		t.Fatalf("应说明为什么保留 got=%v", *logs)
	}

	// 2) 时长不一致（新版其实是别的版本/截断）：保留
	stubDuration(t, map[string]time.Duration{
		plain: 24 * time.Minute,
		sub:   19 * time.Minute,
	})
	r, logs = newRunner()
	r.dropPlainVideo(1, 1, root, actor, "ABC-123", sub, "/usr/bin/ffmpeg")
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("时长不一致时应保留旧档: %v", err)
	}
	if !hasLog(*logs, "warn", "不是同一版，保留") {
		t.Fatalf("应说明为什么保留 got=%v", *logs)
	}

	// 3) 旧档自己带着下载标记（上次中断的残件）：不是可信成品，保留
	marker := partMarkerPath(actor, "ABC-123")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubDuration(t, map[string]time.Duration{
		plain: 24 * time.Minute,
		sub:   24 * time.Minute,
	})
	r, logs = newRunner()
	r.dropPlainVideo(1, 1, root, actor, "ABC-123", sub, "/usr/bin/ffmpeg")
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("带下载标记的旧档不可信，应保留: %v", err)
	}
	if !hasLog(*logs, "warn", "旧档本身不可信") {
		t.Fatalf("应说明为什么保留 got=%v", *logs)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("中文字幕版任何时候都不该被删: %v", err)
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
	r := &Runner{}

	// 本地只有普通版：普通版链接跳过，中文字幕版链接照下
	if err := os.WriteFile(plain, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.findExistingVersion(root, actor, "ABC-123"); got != plain {
		t.Fatalf("普通版应命中 got=%q", got)
	}
	if got := r.findExistingVersion(root, actor, "ABC-123-C"); got != "" {
		t.Fatalf("只有普通版时中文字幕版不应跳过 got=%q", got)
	}

	// 本地两份都在：两种链接都跳过
	if err := os.WriteFile(sub, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.findExistingVersion(root, actor, "ABC-123-C"); got != sub {
		t.Fatalf("中文字幕版应命中 got=%q", got)
	}
	if got := r.findExistingVersion(root, actor, "ABC-123"); got != plain {
		t.Fatalf("普通版应命中已有普通版 got=%q", got)
	}
}

// 快照必须与运行中的状态隔离：演员名是切片，浅拷贝会让前端拿到的
// 快照被后续修改带跑。
func TestSnapshotCopiesActors(t *testing.T) {
	r := New(config.Default(), Hooks{})
	r.state = State{Items: []Item{{Actors: []string{"演员甲"}}}}

	st := r.Snapshot()
	st.Items[0].Actors[0] = "改过"

	if got := r.Snapshot().Items[0].Actors[0]; got != "演员甲" {
		t.Fatalf("快照改动不应回写内部状态 got=%q", got)
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

// 清除已下载之后要能把界面队列一起清空，且推给前端的是空快照而不是 null。
func TestResetClearsQueue(t *testing.T) {
	var got State
	r := New(config.Default(), Hooks{OnState: func(s State) { got = s }})
	r.state = State{Total: 2, Items: []Item{{URL: "u1", Status: StatusDone}, {URL: "u2", Status: StatusFailed}}}

	if err := r.Reset(); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if st := r.Snapshot(); st.Total != 0 || len(st.Items) != 0 || st.Running {
		t.Fatalf("队列应被清空: %+v", st)
	}
	if got.Items == nil {
		t.Fatal("推送的空状态 Items 不应为 nil，前端会当成没拿到数据")
	}
	if len(got.Items) != 0 {
		t.Fatalf("推送的状态应是空的: %+v", got)
	}
}

// 运行中清队列会把列表与正在推进的进度对错位，必须拒绝。
func TestResetRejectedWhileRunning(t *testing.T) {
	r := New(config.Default(), Hooks{})
	r.running = true
	if err := r.Reset(); err == nil {
		t.Fatal("运行中应拒绝清空队列")
	}
}

// 下载前就存在的文件不能被当成「这次写的」——否则上次中断留下的残件
// 会被这次的成功判定认领（GR-01 的另一半）。
func TestProducedVideoOnlyAcceptsFreshWrites(t *testing.T) {
	dir := t.TempDir()
	name := "AUDIT-001"

	// 没有文件
	if got := producedVideo(dir, name, stampCandidates(dir, name)); got != "" {
		t.Fatalf("没有任何文件时不应有产物 got=%q", got)
	}

	// 下载前后一模一样：不算这次的产物
	p := filepath.Join(dir, name+".mp4")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 64), 0o644); err != nil {
		t.Fatal(err)
	}
	before := stampCandidates(dir, name)
	if got := producedVideo(dir, name, before); got != "" {
		t.Fatalf("未变化的旧文件不应算本次产物 got=%q", got)
	}

	// 大小变了（这次重写了）：算
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 128), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := producedVideo(dir, name, before); got != p {
		t.Fatalf("被重写的文件应算本次产物 got=%q want=%q", got, p)
	}

	// 空文件不算成品：dl-re 失败时可能留下 0 字节文件
	if err := os.WriteFile(filepath.Join(dir, "AUDIT-002.mp4"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := producedVideo(dir, "AUDIT-002", stampCandidates(dir, "AUDIT-002")); got != "" {
		t.Fatalf("0 字节不应算产物 got=%q", got)
	}
}

// 假下载器：按 --save-name/--save-dir 落到文件，可选打一行进度。
const fakeDlreScript = `#!/bin/sh
name=""; dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    --save-name) name="$2"; shift 2 ;;
    --save-dir) dir="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$FAKE_PROGRESS" ]; then
  echo "Vid 1920x1080 | 6221 Kbps 1/2 50.00% 1000MB/2.09GB 6.11MBps 00:04:12"
fi
if [ -z "$FAKE_NONE" ]; then
  dd if=/dev/zero of="$dir/$name.mp4" bs=1024 count=16 2>/dev/null
fi
exit 0
`

// 跑一次真实的 downloadPhase，下载器换成上面的脚本。
func runDownloadPhase(t *testing.T, name string, env map[string]string) (*Runner, string, error) {
	t.Helper()
	base := t.TempDir()
	save := filepath.Join(base, "save")
	script := filepath.Join(base, "fake-dlre.sh")
	if err := os.WriteFile(script, []byte(fakeDlreScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	cfg := config.Default()
	cfg.SaveDir = save
	r := New(cfg, Hooks{OnLog: func(string, string) {}})
	r.state = State{Running: true, Total: 1, Items: []Item{
		{URL: "https://jable.tv/v/audit", Site: "jable", Status: StatusPending},
	}}
	r.fileIndex = buildFileIndex(save)
	r.token = 1 // 模拟已开始的这一轮：状态写入要过代次校验

	ts := tools{dlrPath: script, ffmpegPath: "/bin/true"}
	res := scraper.Result{OK: true, VideoName: name, M3U8URL: "https://example/m.m3u8"}
	return r, save, r.downloadPhase(context.Background(), cfg, ts, 0, res, 1)
}

// GR-01 的核心：完成与否由本次实际产物决定。
func TestDownloadPhaseCommitsProducedFile(t *testing.T) {
	const name = "AUDIT-001"

	r, save, err := runDownloadPhase(t, name, nil)
	if err != nil {
		t.Fatalf("正常下载不应报错: %v", err)
	}
	want := filepath.Join(save, name+".mp4")
	it := r.item(0)
	if it.Status != StatusDone || it.Path != want || it.Percent != 100 {
		t.Fatalf("应标完成并回填路径 got status=%s path=%q percent=%v", it.Status, it.Path, it.Percent)
	}
	if _, statErr := os.Stat(doneRecordPath(want)); statErr != nil {
		t.Fatalf("应写完成记录: %v", statErr)
	}
	if _, statErr := os.Stat(partMarkerPath(save, name)); !os.IsNotExist(statErr) {
		t.Fatal("完成记录写好之后才摘标记，此时标记应已不存在")
	}
	// 下一轮必须认这份成品，而不是重下
	if got := r.findExistingVersion(save, save, name); got != want {
		t.Fatalf("下一轮应命中成品 got=%q want=%q", got, want)
	}
}

// 下载器退出 0 但一个文件都没产出：不能标完成，也不能把标记摘掉。
func TestDownloadPhaseRejectsEmptyOutput(t *testing.T) {
	const name = "AUDIT-002"

	r, save, err := runDownloadPhase(t, name, map[string]string{"FAKE_NONE": "1"})
	if err == nil {
		t.Fatal("没有产出时应返回错误，不能标完成")
	}
	if it := r.item(0); it.Status == StatusDone || it.Path != "" {
		t.Fatalf("不应标完成 got status=%s path=%q", it.Status, it.Path)
	}
	if _, statErr := os.Stat(partMarkerPath(save, name)); statErr != nil {
		t.Fatalf("失败时应保留下载标记（下次仍按残件重下）: %v", statErr)
	}
	if _, statErr := os.Stat(doneRecordPath(filepath.Join(save, name+".mp4"))); !os.IsNotExist(statErr) {
		t.Fatal("没有产物就不该有完成记录")
	}
}

// 退出 0 但只写了很小一段：只认退出码与本次产物，不再拿下载器报告的总体积比对。
// 那个数字在分片还没探完时会一路涨（实测 1.94GB 的成品对上一路涨到 5.44GB 的
// 报告值），拿它卡体积只会把下完的片子判成残件。
func TestDownloadPhaseIgnoresReportedTotal(t *testing.T) {
	const name = "AUDIT-003"

	r, save, err := runDownloadPhase(t, name, map[string]string{"FAKE_PROGRESS": "1"})
	if err != nil {
		t.Fatalf("下载器没报错就不该判失败，got=%v", err)
	}
	want := filepath.Join(save, name+".mp4")
	it := r.item(0)
	if it.Status != StatusDone || it.Path != want {
		t.Fatalf("应按完成处理 got status=%s path=%q", it.Status, it.Path)
	}
	if _, statErr := os.Stat(doneRecordPath(want)); statErr != nil {
		t.Fatalf("应写完成记录: %v", statErr)
	}
	if _, statErr := os.Stat(partMarkerPath(save, name)); !os.IsNotExist(statErr) {
		t.Fatal("完成之后标记应已摘掉")
	}
}

// 旧一轮的迟到写入不能落到新队列上：停止后立刻开始新一轮时，
// 预取协程与节流定时器都可能还在跑（GR-03）。
func TestStaleRunTokenIsIgnored(t *testing.T) {
	pushed := 0
	r := New(config.Default(), Hooks{OnState: func(State) { pushed++ }})
	r.state = State{Total: 1, Items: []Item{{URL: "https://jable.tv/v/a", Status: StatusPending}}}
	r.token = 3

	if r.applyPatch(2, 0, func(it *Item) { it.Status = StatusDone }) {
		t.Fatal("旧代次的 applyPatch 应被拒绝")
	}
	r.patch(2, 0, func(it *Item) { it.Status = StatusCancelled })
	r.markRemaining(2, 0, StatusFailed, "旧代次")
	r.updateIndex(2, 1)
	if st := r.Snapshot(); st.Items[0].Status != StatusPending || st.Index != 0 || st.Items[0].Message != "" {
		t.Fatalf("旧代次不应改动状态: %+v", st)
	}
	if pushed != 0 {
		t.Fatalf("旧代次不应推送状态，实际推送 %d 次", pushed)
	}

	// 当前代次照常生效
	r.patch(3, 0, func(it *Item) { it.Status = StatusDone })
	if st := r.Snapshot(); st.Items[0].Status != StatusDone {
		t.Fatalf("当前代次应写入: %+v", st)
	}
	if pushed != 1 {
		t.Fatalf("当前代次应推送一次，实际 %d", pushed)
	}
}

// 同一部片子散在多个演员目录、排序在前的那个是残件时，索引必须跳过它继续找，
// 结果要与完整遍历版一致（GR-04）。旧实现只留同名第一个，直接返回空。
func TestIndexedLookupSkipsBrokenCandidate(t *testing.T) {
	root := t.TempDir()
	broken := filepath.Join(root, "A-演員")
	good := filepath.Join(root, "B-演員")
	for _, d := range []string{broken, good} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const name = "ABC-123"

	// A 目录：上次中断留下的残件（带下载标记，没有完成记录）
	brokenPath := filepath.Join(broken, name+".mp4")
	if err := os.WriteFile(brokenPath, bytes.Repeat([]byte("x"), 64), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partMarkerPath(broken, name), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	// B 目录：有效成品（有完成记录且大小一致）
	goodPath := filepath.Join(good, name+".mp4")
	if err := os.WriteFile(goodPath, bytes.Repeat([]byte("x"), 128), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDoneRecord(goodPath, "https://jable.tv/v/abc-123"); err != nil {
		t.Fatal(err)
	}

	// 索引里两个候选都要在，且顺序与目录优先级一致
	idx := buildFileIndex(root)
	if got := idx[name+".mp4"]; len(got) != 2 || got[0] != brokenPath || got[1] != goodPath {
		t.Fatalf("索引应保留两张同名候选并按目录排序 got=%v", got)
	}

	// 完整遍历版与索引版必须给出同一个答案
	if want := findExisting(root, broken, name); want != goodPath {
		t.Fatalf("完整遍历应命中 B 目录的成品 got=%q", want)
	}
	r := New(config.Default(), Hooks{})
	r.fileIndex = idx
	if got := r.findExistingIndexed(root, broken, name); got != goodPath {
		t.Fatalf("索引版应跳过残件命中成品 got=%q want=%q", got, goodPath)
	}

	// 只有一个残件时仍然是「没有」：不能把残件当成成品
	os.Remove(goodPath)
	os.Remove(doneRecordPath(goodPath))
	r.fileIndex = buildFileIndex(root)
	if got := r.findExistingIndexed(root, broken, name); got != "" {
		t.Fatalf("只剩残件时不应命中 got=%q", got)
	}
}

// BenchmarkStatePush 量化「每次推送都深拷贝全队列 + JSON 序列化」的成本。
// GR-05 里「改成增量事件」要不要做，先看这组数字再决定（当前只有进度合并与行签名）。
func BenchmarkStatePush(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		items := make([]Item, n)
		for i := range items {
			items[i] = Item{
				URL:  "https://jable.tv/videos/abc-" + strconv.Itoa(i),
				Site: "jable", Name: "abc-" + strconv.Itoa(i),
				Status: StatusDownloading, Percent: 42.5,
				Speed: "6.11MBps", ETA: "00:04:12",
				Downloaded: "959.17MB", TotalSize: "2.09GB",
				Actors: []string{"演员甲", "演员乙"},
			}
		}
		st := State{Running: true, Index: n, Total: n, Items: items}
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cp := cloneState(st)
				if _, err := json.Marshal(cp); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
