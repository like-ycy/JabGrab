// Package pipeline 是应用的主流程：读 txt → 去重分类 → 逐条爬取 → 下载。
// 并发固定为 1，爬一个下一个，不重试、不落库。
package pipeline

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"jabgrab/internal/config"
	"jabgrab/internal/downloader"
	"jabgrab/internal/locate"
	"jabgrab/internal/scraper"
)

// Status 是单条任务的状态。
type Status string

const (
	StatusPending     Status = "pending"     // 待处理
	StatusScraping    Status = "scraping"    // 爬取中
	StatusDownloading Status = "downloading" // 下载中
	StatusDone        Status = "done"        // 完成
	StatusSkipped     Status = "skipped"     // 已存在，跳过
	StatusIgnored     Status = "ignored"     // 未匹配任何启用站点
	StatusFailed      Status = "failed"      // 失败
	StatusCancelled   Status = "cancelled"   // 已取消
)

// Item 是列表里的一行。
type Item struct {
	URL        string  `json:"url"`
	Site       string  `json:"site"`
	Name       string  `json:"name"`
	Status     Status  `json:"status"`
	Message    string  `json:"message"`
	Percent    float64 `json:"percent"`
	Speed      string  `json:"speed"`
	ETA        string  `json:"eta"`
	Downloaded string  `json:"downloaded"`
	TotalSize  string  `json:"totalSize"`
}

// State 是推给前端的完整快照。
type State struct {
	Running bool   `json:"running"`
	Index   int    `json:"index"` // 当前处理到第几条，从 1 开始
	Total   int    `json:"total"`
	Items   []Item `json:"items"`
}

// Hooks 是流水线对外的事件出口。
type Hooks struct {
	OnState func(State)
	OnLog   func(level, text string)
}

// Runner 持有一次运行的上下文。
type Runner struct {
	mu      sync.Mutex
	cfg     config.Config
	hooks   Hooks
	state   State
	cancel  context.CancelFunc
	running bool
}

// New 创建 Runner。
func New(cfg config.Config, hooks Hooks) *Runner {
	return &Runner{cfg: cfg, hooks: hooks}
}

// UpdateConfig 在未运行时替换配置。
func (r *Runner) UpdateConfig(cfg config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
}

// Snapshot 返回当前状态副本。
func (r *Runner) Snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneState(r.state)
}

// Running 报告是否正在运行。
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Stop 请求停止：取消 ctx 会杀掉当前爬取/下载进程组。
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Run 执行一轮完整流程，阻塞直到结束或被取消。
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("任务已在运行中")
	}
	cfg := r.cfg
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.state.Running = false
		st := cloneState(r.state)
		r.mu.Unlock()
		cancel()
		r.emitState(st)
	}()

	urls, err := ReadURLs(cfg.TxtPath)
	if err != nil {
		return err
	}
	items := buildItems(cfg, urls)
	r.mu.Lock()
	r.state = State{Running: true, Total: len(items), Items: items}
	st := cloneState(r.state)
	r.mu.Unlock()
	r.emitState(st)

	if len(items) == 0 {
		r.logf("warn", "txt 里没有可处理的 URL")
		return nil
	}

	dlrPath, dlrSource, dlrErr := locate.Dlr(cfg)
	if dlrErr != nil {
		return dlrErr
	}
	// ffmpeg 是 dl-re 的启动期硬依赖：缺了它连下载都不开始（不是只影响混流）。
	// 除了 PATH，也要认下载器同目录（便携部署的常见摆法），否则会误拒可用环境。
	if downloader.FindFFmpeg(filepath.Dir(dlrPath)) == "" {
		return errors.New("未找到 ffmpeg：N_m3u8DL-RE 启动时必须校验 ffmpeg，" +
			"请安装后重试，或把 ffmpeg 放到下载器同目录")
	}
	scrapeArgv, scrapeDir, scrapeErr := locate.Scrape(cfg)
	if scrapeErr != nil {
		return scrapeErr
	}
	r.logf("info", fmt.Sprintf("下载器: %s（%s）", dlrPath, dlrSource))
	r.logf("info", fmt.Sprintf("爬取命令: %s", strings.Join(scrapeArgv, " ")))

	scr := scraper.Scraper{
		Argv:    scrapeArgv,
		Dir:     scrapeDir,
		Timeout: seconds(cfg.ScrapeTimeoutSec),
		Log:     func(line string) { r.logf("debug", line) },
	}

	for i := range items {
		if runCtx.Err() != nil {
			r.markRemaining(i, StatusCancelled, "已取消")
			break
		}
		if items[i].Status == StatusIgnored {
			continue
		}
		r.updateIndex(i + 1)

		if err := r.process(runCtx, cfg, dlrPath, scr, i); err != nil {
			if errors.Is(err, context.Canceled) {
				r.patch(i, func(it *Item) {
					it.Status = StatusCancelled
					it.Message = "已取消"
				})
				r.markRemaining(i+1, StatusCancelled, "已取消")
				break
			}
			r.patch(i, func(it *Item) {
				it.Status = StatusFailed
				it.Message = err.Error()
			})
			r.logf("error", fmt.Sprintf("[%d/%d] %s 失败: %v", i+1, len(items), items[i].URL, err))
		}
	}

	r.logf("info", "本轮结束")
	return nil
}

// process 处理单条：爬取 → 跳过判断 → 下载。
func (r *Runner) process(
	ctx context.Context, cfg config.Config, dlrPath string, scr scraper.Scraper, idx int,
) error {
	item := r.item(idx)
	total := r.total()

	r.patch(idx, func(it *Item) {
		it.Status = StatusScraping
		it.Message = "爬取中"
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 爬取 %s（%s）", idx+1, total, item.URL, item.Site))

	res, err := scr.Scrape(ctx, item.URL, item.Site)
	if err != nil {
		return err
	}
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "爬取失败"
		}
		return errors.New(msg)
	}

	name := downloader.SafeName(res.VideoName)
	if name == "" {
		return errors.New("爬取结果没有视频名")
	}
	r.patch(idx, func(it *Item) { it.Name = name; it.Message = "已拿到 m3u8" })

	// 有演员名就归档到「保存目录/演员名」下（多个演员用横线连接）；
	// 取不到演员名（missav、或页面上没有演员信息）时直接放保存目录根，
	// 不额外套一层「未知演员」，免得所有 missav 全挤在一个目录里。
	actorDir := downloader.ActorDir(res.Actors)
	saveDir := cfg.SaveDir
	if actorDir != "" {
		saveDir = filepath.Join(cfg.SaveDir, actorDir)
		r.logf("info", fmt.Sprintf("[%d/%d] 演员 %s", idx+1, total, strings.Join(res.Actors, "、")))
	}

	if exists := findExisting(cfg.SaveDir, saveDir, name); exists != "" {
		r.patch(idx, func(it *Item) {
			it.Status = StatusSkipped
			it.Message = "已存在: " + filepath.Base(exists)
		})
		r.logf("info", fmt.Sprintf("[%d/%d] 跳过 %s（文件已存在）", idx+1, total, name))
		return nil
	}

	r.patch(idx, func(it *Item) {
		it.Status = StatusDownloading
		it.Message = "下载中"
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 下载 %s → %s", idx+1, total, name, res.M3U8URL))

	opts := downloader.Options{
		Bin:        dlrPath,
		SaveDir:    saveDir,
		SaveName:   name,
		Threads:    cfg.Download.Threads,
		RetryCount: cfg.Download.RetryCount,
		TimeoutSec: cfg.Download.TimeoutSec,
		MaxSpeed:   cfg.Download.MaxSpeed,
		MuxToMP4:   cfg.Download.MuxToMP4,
		FFmpegPath: downloader.FindFFmpeg(filepath.Dir(dlrPath)),
		Headers:    res.Headers,
	}

	err = opts.Run(ctx, res.M3U8URL,
		func(p downloader.Progress) {
			r.patch(idx, func(it *Item) {
				it.Percent = p.Percent
				it.Speed = p.Speed
				it.ETA = p.ETA
				it.Downloaded = p.Downloaded
				it.TotalSize = p.TotalSize
			})
		},
		func(line string) { r.logf("debug", line) },
	)
	if err != nil {
		return err
	}

	r.patch(idx, func(it *Item) {
		it.Status = StatusDone
		it.Percent = 100
		it.Message = "完成"
		it.Speed = ""
		it.ETA = ""
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 完成 %s → %s", idx+1, total, name, saveDir))
	return nil
}

// ReadURLs 读取 txt，忽略空行与 # 注释，去重并保持原顺序。
func ReadURLs(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("未选择待下载 txt 文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 txt 失败: %w", err)
	}
	defer f.Close()

	seen := make(map[string]bool)
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 txt 失败: %w", err)
	}
	return out, nil
}

// buildItems 按已启用站点关键字分类。
func buildItems(cfg config.Config, urls []string) []Item {
	items := make([]Item, 0, len(urls))
	for _, u := range urls {
		it := Item{URL: u, Status: StatusPending}
		if site, ok := cfg.MatchSite(u); ok {
			it.Site = site
		} else {
			it.Status = StatusIgnored
			it.Message = "未匹配任何已启用站点"
		}
		items = append(items, it)
	}
	return items
}

// findExisting 找已经下好的同名成品文件：先看本次要写入的目录，
// 再看保存目录根，最后扫一遍一级子目录（演员目录）。
//
// 后两处是为了兼容「目录变了但片子没变」：改动前下好的文件还在根目录，
// 以及本次没解析出演员（missav 或页面解析失败）、但片子其实已经躺在某个
// 演员目录里。只查目标目录的话，这些都会被当成没下过而重新下一遍。
func findExisting(root, targetDir, name string) string {
	if p := existingVideo(targetDir, name); p != "" {
		return p
	}
	if targetDir != root {
		if p := existingVideo(root, name); p != "" {
			return p
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		sub := filepath.Join(root, e.Name())
		if !e.IsDir() || sub == targetDir {
			continue
		}
		if p := existingVideo(sub, name); p != "" {
			return p
		}
	}
	return ""
}

// existingVideo 检查保存目录下是否已有同名成品文件。
func existingVideo(dir, name string) string {
	for _, ext := range []string{".mp4", ".mkv", ".ts", ".flv", ".webm"} {
		p := filepath.Join(dir, name+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
			return p
		}
	}
	return ""
}

func (r *Runner) item(idx int) Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.state.Items) {
		return Item{}
	}
	return r.state.Items[idx]
}

func (r *Runner) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Total
}

// patch 修改单条并推送状态。
func (r *Runner) patch(idx int, fn func(*Item)) {
	r.mu.Lock()
	if idx < 0 || idx >= len(r.state.Items) {
		r.mu.Unlock()
		return
	}
	fn(&r.state.Items[idx])
	st := cloneState(r.state)
	r.mu.Unlock()
	r.emitState(st)
}

func (r *Runner) updateIndex(idx int) {
	r.mu.Lock()
	r.state.Index = idx
	st := cloneState(r.state)
	r.mu.Unlock()
	r.emitState(st)
}

// markRemaining 把 idx 之后的待处理项标为指定状态。
func (r *Runner) markRemaining(from int, status Status, msg string) {
	r.mu.Lock()
	for i := from; i < len(r.state.Items); i++ {
		if r.state.Items[i].Status == StatusPending || r.state.Items[i].Status == StatusScraping ||
			r.state.Items[i].Status == StatusDownloading {
			r.state.Items[i].Status = status
			r.state.Items[i].Message = msg
		}
	}
	st := cloneState(r.state)
	r.mu.Unlock()
	r.emitState(st)
}

func (r *Runner) emitState(st State) {
	if r.hooks.OnState != nil {
		r.hooks.OnState(st)
	}
}

func (r *Runner) logf(level, text string) {
	if r.hooks.OnLog != nil {
		r.hooks.OnLog(level, text)
	}
}

func cloneState(s State) State {
	cp := s
	cp.Items = make([]Item, len(s.Items))
	copy(cp.Items, s.Items)
	return cp
}

func seconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
