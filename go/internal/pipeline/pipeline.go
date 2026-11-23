// Package pipeline 是应用的主流程：读 txt → 去重分类 → 逐条爬取 → 下载。
// 并发固定为 1，爬一个下一个，不重试、不落库。
package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	StatusFailed      Status = "failed"      // 失败
	StatusCancelled   Status = "cancelled"   // 已取消
)

// Item 是列表里的一行。
type Item struct {
	URL  string `json:"url"`
	Site string `json:"site"`
	// Actors 是爬取到的演员名，未爬取或页面无演员信息时为空。
	// 前端按它渲染「演员」列，省得只能翻日志确认片子归属。
	Actors     []string `json:"actors"`
	Name       string   `json:"name"`
	Status     Status   `json:"status"`
	Message    string   `json:"message"`
	Percent    float64  `json:"percent"`
	Speed      string   `json:"speed"`
	ETA        string   `json:"eta"`
	Downloaded string   `json:"downloaded"`
	TotalSize  string   `json:"totalSize"`
	// Path 是成品视频的绝对路径，只在「完成」与「已存在」时有值。
	// 前端据此决定要不要给这一行放播放按钮；真正启动播放器的是上层。
	Path string `json:"path"`
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
	// Resolve 把「组件名 → 已安装组件的候选路径」注入定位链。
	// 为 nil 时定位只看随包与旧布局，等价于「没装过按需组件」。
	Resolve locate.Resolver
}

// Runner 持有一次运行的上下文。
type Runner struct {
	mu      sync.Mutex
	cfg     config.Config
	hooks   Hooks
	state   State
	cancel  context.CancelFunc
	running bool
	// fileIndex 是一轮开始时建的「文件名 → 路径」索引，
	// 免得每条任务都扫一遍全部演员目录。本轮新落盘的成品要补进去。
	fileIndex map[string]string

	// emitMu 只保护进度节流定时器，与 mu 分开：不能持状态锁等定时器。
	emitMu      sync.Mutex
	pendingEmit *time.Timer
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

// Reset 清空任务列表（不动 txt）。
//
// 用在「清除已下载」之后：txt 里成功的 URL 已经删掉，界面上再挂着上一轮的
// 完成 / 失败状态只会让人以为还有活没干完。运行中不允许清：这一轮还在往里
// 写状态，清完列表与进度必然对不上。
func (r *Runner) Reset() error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("任务正在运行，请先停止")
	}
	r.state = State{}
	st := cloneState(r.state)
	r.mu.Unlock()

	r.stopPendingEmit()
	r.emitState(st)
	return nil
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
		r.stopPendingEmit()
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

	r.logf("info", fmt.Sprintf("扫描到 %d 条去重 URL，可处理 %d 条，忽略 %d 条", len(urls), len(items), len(urls)-len(items)))
	if len(items) == 0 {
		r.logf("warn", "txt 里没有匹配已启用站点的 URL")
		return nil
	}

	t, err := r.resolveTools(cfg)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.fileIndex = buildFileIndex(cfg.SaveDir)
	r.mu.Unlock()

	// 预取深度 1：下载第 N 条时爬第 N+1 条，把开浏览器 + 等页面的时间藏进下载里。
	// m3u8 地址有时效，不能预取更深；停止时 runCtx 会把两个进程组一起杀掉。
	var pending <-chan scrapeOutcome
	startPrefetch := func(i int) <-chan scrapeOutcome {
		ch := make(chan scrapeOutcome, 1)
		go func() {
			res, err := r.scrapePhase(runCtx, t.scr, i)
			ch <- scrapeOutcome{res: res, err: err}
		}()
		return ch
	}

	for i := range items {
		if runCtx.Err() != nil {
			r.markRemaining(i, StatusCancelled, "已取消")
			break
		}
		r.updateIndex(i + 1)

		var res scraper.Result
		var serr error
		if pending != nil {
			o := <-pending
			pending = nil
			res, serr = o.res, o.err
		} else {
			res, serr = r.scrapePhase(runCtx, t.scr, i)
		}
		if serr != nil {
			if errors.Is(serr, context.Canceled) {
				r.patch(i, func(it *Item) {
					it.Status = StatusCancelled
					it.Message = "已取消"
				})
				r.markRemaining(i+1, StatusCancelled, "已取消")
				break
			}
			r.patch(i, func(it *Item) {
				it.Status = StatusFailed
				it.Message = serr.Error()
			})
			r.logf("error", fmt.Sprintf("[%d/%d] %s 失败: %v", i+1, len(items), items[i].URL, serr))
			continue
		}

		if i+1 < len(items) && runCtx.Err() == nil {
			pending = startPrefetch(i+1)
		}

		if derr := r.downloadPhase(runCtx, cfg, t, i, res); derr != nil {
			if errors.Is(derr, context.Canceled) {
				r.patch(i, func(it *Item) {
					it.Status = StatusCancelled
					it.Message = "已取消"
				})
				r.markRemaining(i+1, StatusCancelled, "已取消")
				break
			}
			r.patch(i, func(it *Item) {
				it.Status = StatusFailed
				it.Message = derr.Error()
			})
			r.logf("error", fmt.Sprintf("[%d/%d] %s 失败: %v", i+1, len(items), items[i].URL, derr))
		}
	}

	r.logf("info", "本轮结束")
	return nil
}

// tools 是一轮运行解析好的三件套（ffmpeg 路径 + 下载器 + 爬取器）。
type tools struct {
	dlrPath    string
	ffmpegPath string
	scr        scraper.Scraper
}

// resolveTools 走定位链解析三件套并写日志。Run 与单条重试共用。
func (r *Runner) resolveTools(cfg config.Config) (tools, error) {
	dlrPath, dlrSource, err := locate.Dlr(cfg, locate.From(r.hooks.Resolve, locate.NameDlr)...)
	if err != nil {
		return tools{}, err
	}
	// ffmpeg 是 dl-re 的启动期硬依赖：缺了它连下载都不开始（不是只影响混流）。
	ffmpegPath, ffmpegSource, err := locate.RequireFFmpeg(cfg, []string{filepath.Dir(dlrPath)},
		locate.From(r.hooks.Resolve, locate.NameFFmpeg)...)
	if err != nil {
		return tools{}, err
	}
	scrapeArgv, scrapeDir, scrapeSource, err := locate.ScrapeSource(cfg,
		locate.From(r.hooks.Resolve, locate.NameScraper)...)
	if err != nil {
		return tools{}, err
	}
	r.logf("info", fmt.Sprintf("下载器: %s（%s）", dlrPath, dlrSource))
	r.logf("info", fmt.Sprintf("爬取命令: %s（%s）", strings.Join(scrapeArgv, " "), scrapeSource))
	r.logf("info", fmt.Sprintf("ffmpeg: %s（%s）", ffmpegPath, ffmpegSource))

	return tools{
		dlrPath:    dlrPath,
		ffmpegPath: ffmpegPath,
		scr: scraper.Scraper{
			Argv:    scrapeArgv,
			Dir:     scrapeDir,
			Timeout: seconds(cfg.ScrapeTimeoutSec),
			Log:     func(line string) { r.logf("debug", line) },
		},
	}, nil
}

// scrapeOutcome 是预取协程回传的结果。
type scrapeOutcome struct {
	res scraper.Result
	err error
}

// scrapePhase 爬取单条并把演员 / 视频名落到任务项上。
func (r *Runner) scrapePhase(ctx context.Context, scr scraper.Scraper, idx int) (scraper.Result, error) {
	item := r.item(idx)
	total := r.total()

	r.patch(idx, func(it *Item) {
		it.Status = StatusScraping
		it.Message = "爬取中"
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 爬取 %s（%s）", idx+1, total, item.URL, item.Site))

	res, err := scr.Scrape(ctx, item.URL, item.Site)
	if err != nil {
		return res, err
	}
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "爬取失败"
		}
		return res, errors.New(msg)
	}

	// 演员名先落到任务项上：即使后面因为取不到视频名而失败，
	// 列表里也能看出这条是谁的作品，不用回头翻日志。
	if len(res.Actors) > 0 {
		r.patch(idx, func(it *Item) {
			it.Actors = append([]string(nil), res.Actors...)
		})
	}

	name := downloader.SafeName(res.VideoName)
	if name == "" {
		return res, errors.New("爬取结果没有视频名")
	}
	r.patch(idx, func(it *Item) { it.Name = name; it.Message = "已拿到 m3u8" })
	return res, nil
}

// downloadPhase 跳过判断 → 下载 → 旧档清理 → 回填成品路径。
func (r *Runner) downloadPhase(
	ctx context.Context, cfg config.Config, t tools, idx int, res scraper.Result,
) error {
	total := r.total()
	name := downloader.SafeName(res.VideoName)

	// 有演员名就归档到「保存目录/演员名」下（多个演员用横线连接）；
	// 取不到演员名（missav、或页面上没有演员信息）时直接放保存目录根，
	// 不额外套一层「未知演员」，免得所有 missav 全挤在一个目录里。
	actorDir := downloader.ActorDir(res.Actors)
	saveDir := cfg.SaveDir
	if actorDir != "" {
		saveDir = filepath.Join(cfg.SaveDir, actorDir)
		r.logf("info", fmt.Sprintf("[%d/%d] 演员 %s", idx+1, total, strings.Join(res.Actors, "、")))
	}

	// 下载前先看本地有没有这部片子的成品。结论对用户是要紧的（为什么没下载），
	// 所以命中与未命中都写日志：只在跳过时打一行的话，日志里看不出检查到底做没做，
	// 也看不出命中的是普通版还是中文字幕版。
	if exists := r.findExistingVersion(cfg.SaveDir, saveDir, name); exists != "" {
		// 跳过时不做任何删除：删除只在本次真的下完中文字幕版之后做。
		r.patch(idx, func(it *Item) {
			it.Status = StatusSkipped
			it.Message = "已存在: " + filepath.Base(exists)
			// 跳过的那份也是成品，同样给个播放按钮，不用去目录里翻。
			it.Path = absPath(exists)
		})
		r.logf("info", fmt.Sprintf("[%d/%d] 本地已有成品，跳过下载：%s", idx+1, total, describeFile(exists)))
		return nil
	}
	r.logf("info", fmt.Sprintf("[%d/%d] 本地没有同名成品（含 -C 中文字幕版），开始下载", idx+1, total))

	r.patch(idx, func(it *Item) {
		it.Status = StatusDownloading
		it.Message = "下载中"
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 下载 %s → %s", idx+1, total, name, res.M3U8URL))

	// 先落「下载中」标记再启动：中断后标记还在，同名非空文件就会被判为残件
	// 而不是成品（见 completedVideo）。演员目录首次下载时还不存在（此前由下载器
	// 启动时才创建），先补建目录再写；建目录或写标记失败只降级为旧行为，不拦下载。
	marker := partMarkerPath(saveDir, name)
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		r.logf("warn", fmt.Sprintf("[%d/%d] 创建保存目录失败: %v", idx+1, total, err))
	} else if err := os.WriteFile(marker, []byte(res.M3U8URL), 0o644); err != nil {
		r.logf("warn", fmt.Sprintf("[%d/%d] 写入下载标记失败（不影响下载）: %v", idx+1, total, err))
	}

	opts := downloader.Options{
		Bin:        t.dlrPath,
		SaveDir:    saveDir,
		SaveName:   name,
		Threads:    cfg.Download.Threads,
		RetryCount: cfg.Download.RetryCount,
		TimeoutSec: cfg.Download.TimeoutSec,
		MaxSpeed:   cfg.Download.MaxSpeed,
		MuxToMP4:   cfg.Download.MuxToMP4,
		FFmpegPath: t.ffmpegPath,
		Headers:    res.Headers,
	}

	err := opts.Run(ctx, res.M3U8URL,
		func(p downloader.Progress) {
			r.patchProgress(idx, func(it *Item) {
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

	// 中文字幕版落地后，同名的普通版就是同一部片子的旧档，删掉它，
	// 免得演员目录里同时躺着一份有字幕和一份没字幕的。
	if plain, ok := plainName(name); ok {
		r.dropPlainVideo(idx+1, total, cfg.SaveDir, saveDir, plain)
	}

	// 成品路径要回填给前端：列表里的「播放」按钮靠它。
	// dl-re 的混流结果后缀取决于参数（默认 mp4，也可能保留 mkv/ts），
	// 所以按目录实际存在的文件找，而不是拼一个假想的 .mp4。
	outPath := existingVideo(saveDir, name)
	if outPath == "" {
		outPath = r.findExistingIndexed(cfg.SaveDir, saveDir, name)
	}
	if outPath == "" {
		r.logf("debug", fmt.Sprintf("[%d/%d] 下载已结束，但在 %s 下没找到成品文件", idx+1, total, saveDir))
	}

	// 下载真的完成了才摘标记、写完成记录：此后同名文件按记录校验大小，
	// 中断残件（标记还在、没有记录）不会再被误判成「已存在」。
	_ = os.Remove(marker)
	if outPath != "" {
		r.recordDone(outPath, r.item(idx).URL)
		r.mu.Lock()
		if r.fileIndex != nil {
			r.fileIndex[filepath.Base(outPath)] = outPath
		}
		r.mu.Unlock()
	}

	r.patch(idx, func(it *Item) {
		it.Status = StatusDone
		it.Percent = 100
		it.Message = "完成"
		it.Speed = ""
		it.ETA = ""
		it.Path = absPath(outPath)
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 完成 %s → %s", idx+1, total, name, saveDir))
	return nil
}

// RunOne 只重跑队列里的一条（失败 / 已取消行的「重试」按钮），不动其它行。
func (r *Runner) RunOne(ctx context.Context, idx int) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("任务正在运行，请先停止")
	}
	if idx < 0 || idx >= len(r.state.Items) {
		r.mu.Unlock()
		return errors.New("任务不存在")
	}
	cfg := r.cfg
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.running = true
	r.state.Running = true
	r.state.Index = idx + 1
	r.state.Items[idx].Status = StatusPending
	r.state.Items[idx].Message = ""
	r.state.Items[idx].Percent = 0
	r.state.Items[idx].Speed = ""
	r.state.Items[idx].ETA = ""
	st := cloneState(r.state)
	r.mu.Unlock()
	r.emitState(st)

	defer func() {
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.state.Running = false
		st := cloneState(r.state)
		r.mu.Unlock()
		cancel()
		r.stopPendingEmit()
		r.emitState(st)
	}()

	r.mu.Lock()
	r.fileIndex = buildFileIndex(cfg.SaveDir)
	r.mu.Unlock()

	t, err := r.resolveTools(cfg)
	if err != nil {
		r.patch(idx, func(it *Item) { it.Status = StatusFailed; it.Message = err.Error() })
		return err
	}
	res, err := r.scrapePhase(runCtx, t.scr, idx)
	if err == nil {
		err = r.downloadPhase(runCtx, cfg, t, idx, res)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.patch(idx, func(it *Item) { it.Status = StatusCancelled; it.Message = "已取消" })
		} else {
			r.patch(idx, func(it *Item) { it.Status = StatusFailed; it.Message = err.Error() })
			r.logf("error", fmt.Sprintf("重试第 %d 条失败: %v", idx+1, err))
		}
	}
	return nil
}

// RemoveItem 把一条任务从本轮队列里移除（不动 txt）。运行中不允许。
func (r *Runner) RemoveItem(idx int) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("任务正在运行，请先停止")
	}
	if idx < 0 || idx >= len(r.state.Items) {
		r.mu.Unlock()
		return errors.New("任务不存在")
	}
	r.state.Items = append(r.state.Items[:idx], r.state.Items[idx+1:]...)
	r.state.Total = len(r.state.Items)
	if r.state.Index > r.state.Total {
		r.state.Index = r.state.Total
	}
	st := cloneState(r.state)
	r.mu.Unlock()
	r.emitState(st)
	return nil
}

// dropPlainVideo 删除与中文字幕版同名的普通版旧文件。
//
// 两道门槛，都是为了不把好文件删掉：
//  1. 本地没有中文字幕版 —— 什么都不做；
//  2. 中文字幕版明显小于普通版 —— 当作没下完（见 subtitleComplete）。
//
// 三种结局都写日志（删了 / 没有旧档 / 因为残件保留），日志窗口里要能看出这一步
// 确实跑过，而不是「没打印 = 没执行」。
func (r *Runner) dropPlainVideo(pos, total int, root, saveDir, plain string) {
	plainPath := r.findExistingIndexed(root, saveDir, plain)
	if plainPath == "" {
		// 从没下过普通版是常态，仍留一行：否则用户无法判断清理有没有执行。
		r.logf("info", fmt.Sprintf("[%d/%d] 没有同名的非中文字幕旧档需要清理", pos, total))
		return
	}
	subPath := existingVideo(saveDir, plain+chineseSubSuffix)
	if subPath == "" {
		subPath = r.findExistingIndexed(root, saveDir, plain+chineseSubSuffix)
	}
	if subPath == "" {
		r.logf("warn", fmt.Sprintf(
			"[%d/%d] 找到非中文字幕旧档 %s，但没找到中文字幕版，暂不删除",
			pos, total, describeFile(plainPath)))
		return
	}
	if !subtitleComplete(subPath, plainPath) {
		r.logf("warn", fmt.Sprintf(
			"[%d/%d] 中文字幕版可能未下载完整（%s），暂不删除 %s",
			pos, total, describeFile(subPath), describeFile(plainPath)))
		return
	}

	removed, err := removePlainVideo(root, saveDir, plain)
	for _, p := range removed {
		r.logf("info", fmt.Sprintf("[%d/%d] 已删除同名的非中文字幕旧档：%s", pos, total, p))
	}
	if len(removed) == 0 && err == nil {
		r.logf("info", fmt.Sprintf("[%d/%d] 没有可清理的非中文字幕旧档", pos, total))
	}
	if err != nil {
		// 清理只是顺手收拾旧档，失败了不能让本次下载算失败。
		r.logf("warn", fmt.Sprintf("[%d/%d] 清理非中文字幕旧档失败: %v", pos, total, err))
	}
}

// subtitleMinRatio 是中文字幕版相对普通版的最小体积比（分母）。
// 取 1/2：同片同长度，残件通常远小于正片，正常换版不会低于这个线。
const subtitleMinRatio = 2

// subtitleComplete 判断中文字幕版是不是完整成品。
//
// N_m3u8DL-RE 偶尔在混流失败后仍以 0 退出，留下一个非空的碎片文件：
// 只按「文件存在且大小 > 0」判断，就会把完整的普通版删掉、只剩残件。
// 这里拿普通版的大小当参照，残件不足一半就认为没下完，保留旧档。
func subtitleComplete(subPath, plainPath string) bool {
	sub, err := os.Stat(subPath)
	if err != nil || sub.Size() <= 0 {
		return false
	}
	plain, err := os.Stat(plainPath)
	if err != nil || plain.Size() <= 0 {
		return false
	}
	return sub.Size()*subtitleMinRatio >= plain.Size()
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
		key := urlKey(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 txt 失败: %w", err)
	}
	return out, nil
}

// urlKey 生成去重用的键：去掉 fragment 与结尾斜杠，让
// `.../videos/abc-123` 和 `.../videos/abc-123/` 算作同一行。
//
// 不动 query：不同参数可能真的指向不同资源，宁可多下一次也不误合并。
func urlKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// buildItems 只将匹配已启用站点的 URL 加入队列，列表与进度共用同一批任务。
func buildItems(cfg config.Config, urls []string) []Item {
	items := make([]Item, 0, len(urls))
	for _, u := range urls {
		site, ok := cfg.MatchSite(u)
		if !ok {
			continue
		}
		items = append(items, Item{URL: u, Site: site, Status: StatusPending})
	}
	return items
}

// findExistingVersion 按「本地已有的可接受版本」查成品：
// 普通版链接在中文字幕版已存在时也算下过，中文字幕版则只认自己。
func (r *Runner) findExistingVersion(root, saveDir, name string) string {
	for _, n := range nameCandidates(name) {
		if p := r.findExistingIndexed(root, saveDir, n); p != "" {
			return p
		}
	}
	return ""
}

// findExistingIndexed 与 findExisting 同语义，但一级子目录部分查本轮索引，
// 不再每条任务都 ReadDir 全部演员目录。目标目录与根目录仍实时 stat：
// 本轮刚写进去的文件不在索引里。
func (r *Runner) findExistingIndexed(root, targetDir, name string) string {
	if p := existingVideo(targetDir, name); p != "" {
		return p
	}
	if targetDir != root {
		if p := existingVideo(root, name); p != "" {
			return p
		}
	}
	r.mu.Lock()
	var cand string
	for _, ext := range videoExts {
		if p := r.fileIndex[name+ext]; p != "" {
			cand = p
			break
		}
	}
	r.mu.Unlock()
	if cand != "" {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Size() > 0 && completedVideo(cand, st.Size()) {
			return cand
		}
	}
	return ""
}

// buildFileIndex 扫保存目录根与一级子目录，建「视频文件名 → 路径」索引。
// 根目录先入表，同名时根目录优先（与 findExisting 的查找顺序一致）。
func buildFileIndex(root string) map[string]string {
	idx := make(map[string]string)
	addDir := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			for _, ext := range videoExts {
				if strings.HasSuffix(e.Name(), ext) {
					if _, ok := idx[e.Name()]; !ok {
						idx[e.Name()] = filepath.Join(dir, e.Name())
					}
					break
				}
			}
		}
	}
	addDir(root)
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				addDir(filepath.Join(root, e.Name()))
			}
		}
	}
	return idx
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
// 非空只是必要条件，还要过 completedVideo 的残件判定。
func existingVideo(dir, name string) string {
	for _, ext := range videoExts {
		p := filepath.Join(dir, name+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 && completedVideo(p, st.Size()) {
			return p
		}
	}
	return ""
}

// partMarkerPath 是「下载中」标记：下载开始前写入，完成后摘除。
// 标记还在就说明上次中断了，此时的同名非空文件只能是残件。
func partMarkerPath(dir, name string) string {
	return filepath.Join(dir, "."+name+".jabgrab-part")
}

// doneRecordPath 是成品旁的完成记录（隐藏文件），记下完成时的大小。
func doneRecordPath(videoPath string) string {
	return filepath.Join(filepath.Dir(videoPath), "."+filepath.Base(videoPath)+".jabgrab-done")
}

type doneRecord struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
	At   string `json:"at"`
}

// recordDone 写完成记录。失败只影响残件判定的精度，不影响成品本身。
func (r *Runner) recordDone(videoPath, url string) {
	st, err := os.Stat(videoPath)
	if err != nil {
		return
	}
	data, err := json.Marshal(doneRecord{URL: url, Size: st.Size(), At: time.Now().Format(time.RFC3339)})
	if err != nil {
		return
	}
	if err := os.WriteFile(doneRecordPath(videoPath), data, 0o644); err != nil {
		r.logf("warn", "写入完成记录失败（不影响成品）: "+err.Error())
	}
}

// completedVideo 判断一个非空文件是不是完整成品：
//   - 有完成记录：大小一致才算（记录之后文件又变了 = 残件）；
//   - 没有记录但有下载标记：上次下载中断，残件；
//   - 两者都没有：引入记录机制之前的旧成品，沿用旧行为信任它。
func completedVideo(p string, size int64) bool {
	if data, err := os.ReadFile(doneRecordPath(p)); err == nil {
		var rec doneRecord
		if json.Unmarshal(data, &rec) == nil {
			return rec.Size == size
		}
		return false
	}
	base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	if _, err := os.Stat(partMarkerPath(filepath.Dir(p), base)); err == nil {
		return false
	}
	return true
}

// absPath 尽量把成品路径转成绝对路径。
//
// 送给播放器的路径必须是绝对的：相对路径会按 wails 进程的工作目录解析，
// 用户从 Dock / 开始菜单启动时那个目录与保存目录毫无关系。
func absPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// describeFile 把路径写成日志里的样子：带大小的「路径（1.20GB）」。
// 命中「已存在」时体积是最有用的旁证 —— 一眼看出是正片还是上次中断的残件；
// stat 失败（文件刚被挪走）时退回纯路径，不影响日志本身。
func describeFile(path string) string {
	if size := fileSize(path); size != "" {
		return fmt.Sprintf("%s（%s）", path, size)
	}
	return path
}

// fileSize 返回人类可读的文件大小，取不到或为空文件时返回空串。
func fileSize(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= 0 {
		return ""
	}
	const unit = 1024
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(st.Size())
	i := 0
	for size >= unit && i < len(units)-1 {
		size /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", st.Size())
	}
	return fmt.Sprintf("%.2f%s", size, units[i])
}

// videoExts 是成品视频可能的后缀，取决于 dl-re 的混流结果。
var videoExts = []string{".mp4", ".mkv", ".ts", ".flv", ".webm"}

// chineseSubSuffix 是中文（中文字幕）版的命名后缀，由爬取侧拼接。
const chineseSubSuffix = "-C"

// plainName 去掉中文字幕后缀，返回对应的普通版名字。
// 不是中文字幕版时 ok=false。
func plainName(name string) (string, bool) {
	base, found := strings.CutSuffix(name, chineseSubSuffix)
	base = strings.TrimSpace(base)
	if !found || base == "" {
		return "", false
	}
	return base, true
}

// nameCandidates 返回「本地哪些文件算这部片子已经下过」。
//
// 方向是有讲究的，中文字幕版和普通版不能一概而论：
//   - 本次是普通版：本地有普通版或中文字幕版都算下过，没必要再下一份更差的。
//   - 本次是中文字幕版：只认中文字幕版。本地只有普通版时要照下，
//     下完再把普通版删掉（见 process 里的清理），否则升级永远进不来。
func nameCandidates(name string) []string {
	if _, ok := plainName(name); ok {
		return []string{name}
	}
	return []string{name, name + chineseSubSuffix}
}

// removePlainVideo 删除普通版（不带 -C）的成品文件。
//
// 只有本地确实存在中文字幕版时才动手：否则删掉普通版等于把这部片子唯一的
// 成品删没了。搜索范围与 findExisting 一致（目标演员目录 → 保存目录根 →
// 其它一级子目录），只删成品视频文件（非目录、大小 > 0）。
// 返回已删除的路径；第一个删除错误一并返回（其余继续尝试）。
func removePlainVideo(root, saveDir, plain string) ([]string, error) {
	if plain == "" || findExisting(root, saveDir, plain+chineseSubSuffix) == "" {
		return nil, nil
	}

	dirs := []string{saveDir}
	if saveDir != root {
		dirs = append(dirs, root)
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			sub := filepath.Join(root, e.Name())
			if e.IsDir() && sub != saveDir {
				dirs = append(dirs, sub)
			}
		}
	}

	var (
		removed  []string
		firstErr error
	)
	for _, dir := range dirs {
		for _, ext := range videoExts {
			p := filepath.Join(dir, plain+ext)
			st, err := os.Stat(p)
			if err != nil || st.IsDir() || st.Size() == 0 {
				continue
			}
			if err := os.Remove(p); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			// 完成记录与下载标记跟着成品走，留着会干扰下一次的残件判定。
			_ = os.Remove(doneRecordPath(p))
			_ = os.Remove(partMarkerPath(dir, plain))
			removed = append(removed, p)
		}
	}
	return removed, firstErr
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

// patch 修改单条并立即推送（状态跃迁用）。
func (r *Runner) patch(idx int, fn func(*Item)) {
	r.applyPatch(idx, fn)
	r.stopPendingEmit()
	r.emitState(r.Snapshot())
}

// patchProgress 供高频进度帧使用：状态立即写入，推送合并节流。
// 否则每一帧都「深拷贝全量快照 → JSON 序列化 → 前端整表重建」，
// 列表一大就是 O(任务数 × 帧率) 的放大链路。
func (r *Runner) patchProgress(idx int, fn func(*Item)) {
	r.applyPatch(idx, fn)
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.pendingEmit != nil {
		return
	}
	r.pendingEmit = time.AfterFunc(150*time.Millisecond, func() {
		r.emitMu.Lock()
		r.pendingEmit = nil
		r.emitMu.Unlock()
		r.emitState(r.Snapshot())
	})
}

func (r *Runner) applyPatch(idx int, fn func(*Item)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.state.Items) {
		return
	}
	fn(&r.state.Items[idx])
}

func (r *Runner) stopPendingEmit() {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.pendingEmit != nil {
		r.pendingEmit.Stop()
		r.pendingEmit = nil
	}
}

func (r *Runner) updateIndex(idx int) {
	r.mu.Lock()
	r.state.Index = idx
	st := cloneState(r.state)
	r.mu.Unlock()
	r.stopPendingEmit()
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
	r.stopPendingEmit()
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
	// Actors 是切片，浅拷贝会与运行中的状态共用底层数组：
	// 单独复制一份，让快照真正不可变。
	for i := range cp.Items {
		if len(cp.Items[i].Actors) > 0 {
			cp.Items[i].Actors = append([]string(nil), cp.Items[i].Actors...)
		}
	}
	return cp
}

func seconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
