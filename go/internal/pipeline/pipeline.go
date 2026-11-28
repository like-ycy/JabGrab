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
	"runtime"
	"strings"
	"sync"
	"time"

	"jabgrab/internal/config"
	"jabgrab/internal/downloader"
	"jabgrab/internal/locate"
	"jabgrab/internal/proc"
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

// runToken 标识一轮运行（Run 或单条重试）。
//
// 预取协程与节流定时器可能比一轮运行活得更久：停止后立刻开始新队列时，
// 它们的写入会落到新队列的同一个下标上。所有写状态的入口都带代次，
// 对不上就丢弃 —— 这比「等它们一定退干净」更可靠，等待只是尽力而为。
type runToken uint64

// Runner 持有一次运行的上下文。
type Runner struct {
	mu      sync.Mutex
	cfg     config.Config
	hooks   Hooks
	state   State
	cancel  context.CancelFunc
	running bool
	// token 是当前运行代次，每轮开始与结束都自增。
	token runToken
	// fileIndex 是一轮开始时建的「文件名 → 候选路径列表」索引，
	// 免得每条任务都扫一遍全部演员目录。同名文件可能有多个（不同演员目录），
	// 只留第一个的话，先遇到残件就把后面目录里的成品一起藏掉了（GR-04）。
	// 本轮新落盘的成品要补进去。
	fileIndex map[string][]string

	// emitMu 只保护进度节流定时器，与 mu 分开：不能持状态锁等定时器。
	emitMu      sync.Mutex
	pendingEmit *time.Timer

	// children 是当前在跑的受管子进程 pid（进程组号）。
	//
	// 爬取与下载可能同时各有一个（预取第 N+1 条时正在下第 N 条），所以是集合
	// 而不是单个字段。「停止」直接照它杀组，不等 ctx 取消经 exec 回调绕一圈：
	// 浏览器与被杀到一半的下载器多活一秒，就多写一秒盘、多留一秒孤儿进程。
	children map[int]bool
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

// Stop 请求停止：取消 ctx，并立即杀掉正在跑的爬取 / 下载进程组。
//
// 两件事都要做。cancel 会经由 exec 的取消回调杀进程组，但那是异步的：回调
// 要等调度，dl-re 还会先把管道里的缓冲吐完。用户在「停止」上按下之后紧跟着
// 打开活动监视器，看到的就是「已经点了停止、N_m3u8DL-RE 还在下」。
// 这里按记下的 pid 直接对进程组下手，杀完才算数；cancel 仍然要发，
// 否则流水线不知道这一轮已经结束（各阶段的返回值得靠 ctx 才算得出「已取消」）。
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	pids := make([]int, 0, len(r.children))
	for pid := range r.children {
		pids = append(pids, pid)
	}
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// 顺序有意为之：先 cancel 断掉启动下一个子进程的可能，再逐个杀组。
	// 杀组是幂等的（进程已退出时视为成功），与取消回调重复无害。
	// 真杀不掉时留一行日志：那意味着用户按了停止、进程还在，事后要查得出来。
	for _, pid := range pids {
		if err := proc.KillPID(pid); err != nil {
			r.logf("warn", fmt.Sprintf("停止时未能杀掉进程组 %d: %v", pid, err))
		}
	}
}

// beginChild 记下一个刚起来的受管子进程。
func (r *Runner) beginChild(pid int) {
	if pid <= 0 {
		return
	}
	r.mu.Lock()
	if r.children == nil {
		r.children = make(map[int]bool, 2)
	}
	r.children[pid] = true
	r.mu.Unlock()
}

// endChild 划掉一个已经收尾的受管子进程。
func (r *Runner) endChild(pid int) {
	r.mu.Lock()
	delete(r.children, pid)
	r.mu.Unlock()
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
	r.token++
	tok := r.token
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.state.Running = false
		st := cloneState(r.state)
		// 这一轮到此为止：之后任何迟到的写入（没等到收尾的预取协程、
		// 还没触发的节流定时器）都按代次丢弃，不会写进下一轮的队列。
		r.token++
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

	// 索引在锁外构建：慢盘上扫一遍演员目录可能要几百毫秒，
	// 持锁做 I/O 会连带把 Snapshot / Stop 一起堵住（GR-05）。
	// 建完再回锁里校验代次，确认这一轮还没被叫停。
	idx := buildFileIndex(cfg.SaveDir)
	r.mu.Lock()
	if r.token == tok {
		r.fileIndex = idx
	}
	r.mu.Unlock()

	// 预取深度 1：下载第 N 条时爬第 N+1 条，把开浏览器 + 等页面的时间藏进下载里。
	// m3u8 地址有时效，不能预取更深；停止时 runCtx 会把两个进程组一起杀掉。
	var (
		pending  <-chan scrapeOutcome
		prefetch sync.WaitGroup
	)
	startPrefetch := func(i int) <-chan scrapeOutcome {
		ch := make(chan scrapeOutcome, 1)
		prefetch.Add(1)
		go func() {
			defer prefetch.Done()
			res, err := r.scrapePhase(runCtx, t.scr, i, tok)
			ch <- scrapeOutcome{res: res, err: err}
		}()
		return ch
	}

	for i := range items {
		if runCtx.Err() != nil {
			r.markRemaining(tok, i, StatusCancelled, "已取消")
			break
		}
		r.updateIndex(tok, i+1)

		var res scraper.Result
		var serr error
		if pending != nil {
			o := <-pending
			pending = nil
			res, serr = o.res, o.err
		} else {
			res, serr = r.scrapePhase(runCtx, t.scr, i, tok)
		}
		if serr != nil {
			if errors.Is(serr, context.Canceled) {
				r.patch(tok, i, func(it *Item) {
					it.Status = StatusCancelled
					it.Message = "已取消"
				})
				r.markRemaining(tok, i+1, StatusCancelled, "已取消")
				break
			}
			r.patch(tok, i, func(it *Item) {
				it.Status = StatusFailed
				it.Message = serr.Error()
			})
			r.logf("error", fmt.Sprintf("[%d/%d] %s 失败: %v", i+1, len(items), items[i].URL, serr))
			continue
		}

		if i+1 < len(items) && runCtx.Err() == nil {
			pending = startPrefetch(i + 1)
		}

		if derr := r.downloadPhase(runCtx, cfg, t, i, res, tok); derr != nil {
			if errors.Is(derr, context.Canceled) {
				r.patch(tok, i, func(it *Item) {
					it.Status = StatusCancelled
					it.Message = "已取消"
				})
				r.markRemaining(tok, i+1, StatusCancelled, "已取消")
				break
			}
			r.patch(tok, i, func(it *Item) {
				it.Status = StatusFailed
				it.Message = derr.Error()
			})
			r.logf("error", fmt.Sprintf("[%d/%d] %s 失败: %v", i+1, len(items), items[i].URL, derr))
		}
	}

	// 预取协程必须收尾再离开循环：runCtx 已取消，它杀掉进程组后很快返回。
	// 等待有界，免得「停止」被拖住；真没退干净也不会污染状态——代次已经挡住了。
	if !waitTimeout(&prefetch, prefetchGrace) {
		r.logf("warn", "预取任务未在预期时间内退出，已放行；它的写入会按代次丢弃")
	}

	r.logf("info", "本轮结束")
	return nil
}

// prefetchGrace 是停止后等待预取协程收尾的上限。
const prefetchGrace = 5 * time.Second

// waitTimeout 等 wg 结束，最多等 d；超时返回 false（不会留下后台 goroutine 泄漏，
// 那个等待协程本身会在 wg 结束时退出）。
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
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
func (r *Runner) scrapePhase(ctx context.Context, scr scraper.Scraper, idx int, tok runToken) (scraper.Result, error) {
	item := r.item(idx)
	total := r.total()

	r.patch(tok, idx, func(it *Item) {
		it.Status = StatusScraping
		// 说明文字留空：状态列已经有蓝色「爬取中」标签，再补一行灰色「爬取中」
		// 只是把同一句话说两遍，看着像两个不同的状态。
		it.Message = ""
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 爬取 %s（%s）", idx+1, total, item.URL, item.Site))

	// 记下爬取进程（含它拉起的浏览器）：按下「停止」时直接杀这个进程组。
	var child int
	scr.OnStart = func(pid int) {
		child = pid
		r.beginChild(pid)
	}
	defer func() {
		if child > 0 {
			r.endChild(child)
		}
	}()

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
		r.patch(tok, idx, func(it *Item) {
			it.Actors = append([]string(nil), res.Actors...)
		})
	}

	name := downloader.SafeName(res.VideoName)
	if name == "" {
		return res, errors.New("爬取结果没有视频名")
	}
	r.patch(tok, idx, func(it *Item) { it.Name = name })
	return res, nil
}

// downloadPhase 跳过判断 → 下载 → 旧档清理 → 回填成品路径。
func (r *Runner) downloadPhase(
	ctx context.Context, cfg config.Config, t tools, idx int, res scraper.Result, tok runToken,
) error {
	total := r.total()
	name := downloader.SafeName(res.VideoName)

	// 有演员名就归档到「保存目录/演员名」下（多个演员用横线连接）；
	// 取不到演员名（页面上没有演员信息或解析失败）时直接放保存目录根，
	// 不额外套一层「未知演员」。
	actorDir := downloader.ActorDir(res.Actors)
	saveDir := cfg.SaveDir
	if actorDir != "" {
		saveDir = filepath.Join(cfg.SaveDir, actorDir)
		r.logf("info", fmt.Sprintf("[%d/%d] 演员 %s", idx+1, total, strings.Join(res.Actors, "、")))
	}

	// 下载前先看本地有没有这部片子的成品。结论对用户是要紧的（为什么没下载），
	// 所以命中与未命中都写日志：只在跳过时打一行的话，日志里看不出检查到底做没做，
	// 也看不出命中的是普通版、中文字幕版还是无码版。
	if exists := r.findExistingVersion(cfg.SaveDir, saveDir, name); exists != "" {
		// 跳过时不做任何删除：删除只在本次真的下完并验证 -C/-U 版之后做。
		r.patch(tok, idx, func(it *Item) {
			it.Status = StatusSkipped
			it.Message = "已存在: " + filepath.Base(exists)
			// 跳过的那份也是成品，同样给个播放按钮，不用去目录里翻。
			it.Path = absPath(exists)
		})
		r.logf("info", fmt.Sprintf("[%d/%d] 本地已有成品，跳过下载：%s", idx+1, total, describeFile(exists)))
		return nil
	}
	r.logf("info", fmt.Sprintf("[%d/%d] 本地没有可接受的同名成品，开始下载", idx+1, total))

	r.patch(tok, idx, func(it *Item) {
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

	// 记下下载前的同名文件状态：下载结束后靠「前后两次 stat 的差」判断哪些是这次
	// 写出来的。不拿文件时间与墙上时钟比——网络盘的时钟有偏差，那种比法会把刚下
	// 完的成品判成旧的（下文 producedVideo）。
	before := stampCandidates(saveDir, name)

	// 当前下载器进程的 pid，用来在收尾时从「停止」的射程里划掉。
	var child int
	opts := downloader.Options{
		Bin:           t.dlrPath,
		SaveDir:       saveDir,
		SaveName:      name,
		Threads:       cfg.Download.Threads,
		RetryCount:    cfg.Download.RetryCount,
		TimeoutSec:    cfg.Download.TimeoutSec,
		MaxSpeed:      cfg.Download.MaxSpeed,
		MuxToMP4:      cfg.Download.MuxToMP4,
		FFmpegPath:    t.ffmpegPath,
		Headers:       res.Headers,
		CurlTransport: runtime.GOOS == "windows" && r.item(idx).Site == "missav",
		// 记下下载器进程：按下「停止」时直接杀这个进程组（含它派生的 ffmpeg）。
		OnStart: func(pid int) {
			child = pid
			r.beginChild(pid)
		},
	}

	// 下载器自己报告的总体积在「分片还没探完」时会一直往上跳，拿它比对成品体积
	// 会把已经下完的片子判成残件（实测 1.94GB 的成品对上一路涨到 5.44GB 的报告值），
	// 所以这里不做体积校验：下没下完只认下载器的退出码与本次实际产物。
	err := opts.Run(ctx, res.M3U8URL,
		func(p downloader.Progress) {
			r.patchProgress(tok, idx, func(it *Item) {
				it.Percent = p.Percent
				it.Speed = p.Speed
				it.ETA = p.ETA
				it.Downloaded = p.Downloaded
				it.TotalSize = p.TotalSize
			})
		},
		func(line string) { r.logf("debug", line) },
	)
	if child > 0 {
		r.endChild(child)
	}
	if err != nil {
		// 失败时标记留着：同名文件下次仍按残件处理，不会被当成成品信任。
		return err
	}

	// 成功与否由本次实际产物决定，不由退出码决定。
	// 这里必须用 producedVideo 而不是 existingVideo/findExistingIndexed：下载前写的
	// part 标记还在，completedVideo 会把刚下完的文件判成残件，永远找不到。
	outPath := producedVideo(saveDir, name, before)
	if outPath == "" {
		return errors.New("下载器已退出，但没有找到本次写出的成品文件；已保留下载标记，下次仍按残件重下")
	}

	// 先写完成记录、再摘标记：顺序反了就等于把这份文件降级成「既无记录也无标记
	// 的旧成品」，completedVideo 的兼容分支会无条件信任它，残件从此再也认不出来。
	if err := writeDoneRecord(outPath, r.item(idx).URL); err != nil {
		return fmt.Errorf("成品已下完，但写完成记录失败: %w", err)
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		r.logf("warn", fmt.Sprintf("[%d/%d] 移除下载标记失败（完成记录已就位，不影响判定）: %v", idx+1, total, err))
	}
	r.mu.Lock()
	if r.fileIndex != nil && r.token == tok {
		base := filepath.Base(outPath)
		old := r.fileIndex[base]
		list := make([]string, 0, len(old)+1)
		list = append(list, outPath)
		for _, p := range old {
			if p != outPath {
				list = append(list, p)
			}
		}
		r.fileIndex[base] = list
	}
	r.mu.Unlock()

	// 新版到这里才算「已提交」。只有已提交的 -C/-U 产物才允许触发普通版旧档清理，
	// 清理时也直接用它当参照，不再去目录里重新找一遍。
	if plain, ok := plainName(name); ok {
		r.dropPlainVideo(idx+1, total, cfg.SaveDir, saveDir, plain, outPath, t.ffmpegPath)
	}

	r.patch(tok, idx, func(it *Item) {
		it.Status = StatusDone
		it.Percent = 100
		it.Message = "完成"
		it.Speed = ""
		it.ETA = ""
		it.Path = absPath(outPath)
	})
	r.logf("info", fmt.Sprintf("[%d/%d] 完成 %s → %s", idx+1, total, name, outPath))
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
	r.token++
	tok := r.token
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
		r.token++
		r.mu.Unlock()
		cancel()
		r.stopPendingEmit()
		r.emitState(st)
	}()

	// 索引在锁外构建，回锁里校验代次后再替换（同 Run）。
	idxFiles := buildFileIndex(cfg.SaveDir)
	r.mu.Lock()
	if r.token == tok {
		r.fileIndex = idxFiles
	}
	r.mu.Unlock()

	t, err := r.resolveTools(cfg)
	if err != nil {
		r.patch(tok, idx, func(it *Item) { it.Status = StatusFailed; it.Message = err.Error() })
		return err
	}
	res, err := r.scrapePhase(runCtx, t.scr, idx, tok)
	if err == nil {
		err = r.downloadPhase(runCtx, cfg, t, idx, res, tok)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.patch(tok, idx, func(it *Item) { it.Status = StatusCancelled; it.Message = "已取消" })
		} else {
			r.patch(tok, idx, func(it *Item) { it.Status = StatusFailed; it.Message = err.Error() })
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

// dropPlainVideo 删除与 -C/-U 新版同名的普通版旧文件。
//
// 前提变了：newPath 必须是**本次已提交**的新版（记录写完、标记已摘），
// 由 downloadPhase 直接传进来，不再去目录里重新查找——重新查找会把别的目录里
// 长得像成品的残件也当成参照。
//
// 每个候选单独过两道门，任何一步信息不足都保留旧档：
//  1. 它自己是可信成品（completedVideo：有记录且大小一致，或旧版无标记）；
//  2. 与新版时长一致（ffmpeg 读容器头，容差见 sameLength）。
//
// 体积不再作为完整性证据：高码率截断的新版完全可能超过旧档的一半，
// 靠比例判断会把好文件删掉。删错一部片的代价远高于多留一份，所以默认保留。
//
// 三种结局都写日志（删了 / 没有旧档 / 因为不可信保留），日志窗口里要能看出这一步
// 确实跑过，而不是「没打印 = 没执行」。
func (r *Runner) dropPlainVideo(pos, total int, root, saveDir, plain, newPath, ffmpeg string) {
	cands := plainCandidates(root, saveDir, plain)
	if len(cands) == 0 {
		// 从没下过普通版是常态，仍留一行：否则用户无法判断清理有没有执行。
		r.logf("info", fmt.Sprintf("[%d/%d] 没有同名的普通版旧档需要清理", pos, total))
		return
	}

	// 新版时长只读一次：多个候选共用同一个参照。
	newDur, newErr := mediaDurationFn(ffmpeg, newPath)
	if newErr != nil {
		r.logf("warn", fmt.Sprintf(
			"[%d/%d] 读不到新版时长（%v），无法确认是否为完整替代，%d 个旧档全部保留：%s",
			pos, total, newErr, len(cands), strings.Join(cands, "、")))
		return
	}

	removed := 0
	for _, p := range cands {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Size() == 0 {
			continue
		}
		if !completedVideo(p, st.Size()) {
			r.logf("warn", fmt.Sprintf("[%d/%d] 旧档本身不可信（无完成记录或有下载标记），保留：%s", pos, total, p))
			continue
		}
		dur, derr := mediaDurationFn(ffmpeg, p)
		if derr != nil {
			r.logf("warn", fmt.Sprintf("[%d/%d] 读不到旧档时长（%v），保留：%s", pos, total, derr, p))
			continue
		}
		if !sameLength(newDur, dur) {
			r.logf("warn", fmt.Sprintf(
				"[%d/%d] 旧档时长 %s 与新版 %s 不一致，不是同一版，保留：%s",
				pos, total, dur.Round(time.Second), newDur.Round(time.Second), p))
			continue
		}
		if err := os.Remove(p); err != nil {
			// 清理只是顺手收拾旧档，失败了不能让本次下载算失败。
			r.logf("warn", fmt.Sprintf("[%d/%d] 删除旧档失败 %s: %v", pos, total, p, err))
			continue
		}
		// 完成记录与下载标记跟着成品走，留着会干扰下一次的残件判定。
		_ = os.Remove(doneRecordPath(p))
		_ = os.Remove(partMarkerPath(filepath.Dir(p), plain))
		removed++
		r.logf("info", fmt.Sprintf("[%d/%d] 已删除同名的普通版旧档：%s", pos, total, p))
	}
	if removed == 0 {
		r.logf("info", fmt.Sprintf("[%d/%d] 没有可清理的普通版旧档（%d 个候选均未通过校验）", pos, total, len(cands)))
	}
}

// sameLength 判断两份媒体是不是同一个长度。
//
// 容差 3 秒：不同封装与切点会带来亚秒级差异，而真正的「不同版本」相差远不止 3 秒。
func sameLength(a, b time.Duration) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 3*time.Second
}

// plainCandidates 列出所有同名普通版候选：目标目录 → 保存目录根 → 其它一级子目录。
// 只收集路径，删不删由调用方逐条判定（不再「同名即删」）。
func plainCandidates(root, saveDir, plain string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(dir string) {
		for _, ext := range videoExts {
			p := filepath.Join(dir, plain+ext)
			if seen[p] {
				continue
			}
			if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
				seen[p] = true
				out = append(out, p)
			}
		}
	}

	add(saveDir)
	if saveDir != root {
		add(root)
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(root, e.Name())
			if sub == saveDir {
				continue
			}
			add(sub)
		}
	}
	return out
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
// 普通版接受已完成的 -C/-U；-C 与 -U 各自只认自己，互不替代。
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
	// 候选按后缀优先级（mp4 先）再按目录优先级排列；锁内只拷一份切片出来，
	// stat 与残件判定都在锁外做（GR-05：别拿着主锁等磁盘）。
	r.mu.Lock()
	var cands []string
	for _, ext := range videoExts {
		if list := r.fileIndex[name+ext]; len(list) > 0 {
			cands = append(cands, list...)
		}
	}
	r.mu.Unlock()

	for _, cand := range cands {
		// 逐个实时校验：索引只记「哪里可能有」，完成状态会变（残件转正、被删、被替换），
		// 不能缓存在索引里。第一个不可信就继续找下一个，而不是直接返回空。
		st, err := os.Stat(cand)
		if err != nil || st.IsDir() || st.Size() <= 0 {
			continue
		}
		if completedVideo(cand, st.Size()) {
			return cand
		}
	}
	return ""
}

// buildFileIndex 扫保存目录根与一级子目录，建「视频文件名 → 路径」索引。
// 根目录先入表，同名时根目录优先（与 findExisting 的查找顺序一致）。
func buildFileIndex(root string) map[string][]string {
	idx := make(map[string][]string)
	addDir := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !hasVideoExt(e.Name()) {
				continue
			}
			// 同名文件全部入表，按「根目录 → 一级子目录」的顺序追加，
			// 查找时逐个校验，跳过失效的那个继续往下（GR-04）。
			idx[e.Name()] = append(idx[e.Name()], filepath.Join(dir, e.Name()))
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
// 以及本次没解析出演员（页面缺少演员信息或解析失败）、但片子其实已经躺在某个
// 演员目录里。只查目标目录的话，这些都会被当成没下过而重新下一遍。
//
// 生产路径走 findExistingIndexed（每轮建一次索引，不逐条 ReadDir）。
// 这里保留完整遍历版，作为索引版的语义参照：GR-04 的验收要求对同一棵目录树
// 比较两者的结果集合，有这份实现才比得出来。
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

// writeDoneRecord 写完成记录，原子落盘。
//
// 调用方必须把它当成「本次是否成功」的一部分：写不进去就让这一条保持失败，
// 不能退回「没有记录也信任」的旧口径。半截 JSON 会让 completedVideo 判成残件，
// 方向是安全的，但会白下一遍，所以先写临时文件再改名。
func writeDoneRecord(videoPath, url string) error {
	st, err := os.Stat(videoPath)
	if err != nil {
		return err
	}
	data, err := json.Marshal(doneRecord{URL: url, Size: st.Size(), At: time.Now().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	return writeFileAtomic(doneRecordPath(videoPath), data, 0o644)
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
	return humanSize(st.Size())
}

// humanSize 把字节数写成人话，日志与错误信息共用同一套换算。
func humanSize(n int64) string {
	if n <= 0 {
		return "0B"
	}
	const unit = 1024
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(n)
	i := 0
	for size >= unit && i < len(units)-1 {
		size /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.2f%s", size, units[i])
}

// videoExts 是成品视频可能的后缀，取决于 dl-re 的混流结果。
var videoExts = []string{".mp4", ".mkv", ".ts", ".flv", ".webm"}

// hasVideoExt 判断文件名是不是成品视频后缀。
func hasVideoExt(name string) bool {
	for _, ext := range videoExts {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// enhancedSuffixes 是可替代普通版的独立版本后缀；两种增强版本互不替代。
var enhancedSuffixes = []string{"-C", "-U"}

// plainName 返回 -C/-U 对应的普通版名字，只剥离最末一个后缀。
func plainName(name string) (string, bool) {
	for _, suffix := range enhancedSuffixes {
		if base, found := strings.CutSuffix(name, suffix); found && strings.TrimSpace(base) != "" {
			return strings.TrimSpace(base), true
		}
	}
	return "", false
}

// nameCandidates 与下载后的普通版清理共享后缀定义：普通版接受任一新版，
// -C 与 -U 分别只认自己，本地只有普通版时继续下载，完成并验证后再清理。
func nameCandidates(name string) []string {
	if _, ok := plainName(name); ok {
		return []string{name}
	}
	candidates := []string{name}
	for _, suffix := range enhancedSuffixes {
		candidates = append(candidates, name+suffix)
	}
	return candidates
}

// fileStamp 是下载前对同名文件的快照，用来判断下载后哪些文件被这次重写过。
type fileStamp struct {
	size int64
	mod  time.Time
}

// stampCandidates 记录目标目录下同名各后缀视频的当前状态。
//
// 它与 producedVideo 配对使用：两次 stat 都来自同一个文件系统，
// 网络盘上客户端与服务端时钟有偏差也不影响比较结果。
func stampCandidates(dir, name string) map[string]fileStamp {
	stamps := make(map[string]fileStamp, len(videoExts))
	for _, ext := range videoExts {
		p := filepath.Join(dir, name+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			stamps[p] = fileStamp{size: st.Size(), mod: st.ModTime()}
		}
	}
	return stamps
}

// producedVideo 找本次下载写出的成品：后缀按 videoExts 顺序，
// 只有「下载前不存在」或「大小/时间变了」的文件才算数。
//
// 只按「存在且非空」判断会把上次中断留下的同名残件算成这次的成功 ——
// 那正是 GR-01 里「找不到文件仍标记完成」的另一半。
func producedVideo(dir, name string, before map[string]fileStamp) string {
	for _, ext := range videoExts {
		p := filepath.Join(dir, name+ext)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Size() == 0 {
			continue
		}
		if old, ok := before[p]; ok && old.size == st.Size() && old.mod.Equal(st.ModTime()) {
			continue // 下载前后一模一样：不是这次写的
		}
		return p
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

// patch 修改单条并立即推送（状态跃迁用）。
// 代次对不上（上一轮遗留的迟到写入）直接丢弃，不碰新一轮的队列。
func (r *Runner) patch(tok runToken, idx int, fn func(*Item)) {
	if !r.applyPatch(tok, idx, fn) {
		return
	}
	r.stopPendingEmit()
	r.emitState(r.Snapshot())
}

// patchProgress 供高频进度帧使用：状态立即写入，推送合并节流。
// 否则每一帧都「深拷贝全量快照 → JSON 序列化 → 前端整表重建」，
// 列表一大就是 O(任务数 × 帧率) 的放大链路。
func (r *Runner) patchProgress(tok runToken, idx int, fn func(*Item)) {
	if !r.applyPatch(tok, idx, fn) {
		return
	}
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.pendingEmit != nil {
		return
	}
	r.pendingEmit = time.AfterFunc(150*time.Millisecond, func() {
		r.emitMu.Lock()
		r.pendingEmit = nil
		r.emitMu.Unlock()
		// 定时器可能比一轮运行活得久：跨代次就别推了，下一轮自己会推。
		if !r.sameToken(tok) {
			return
		}
		r.emitState(r.Snapshot())
	})
}

// applyPatch 在锁内校验代次与下标后写入，返回是否真的写了。
func (r *Runner) applyPatch(tok runToken, idx int, fn func(*Item)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token != tok || idx < 0 || idx >= len(r.state.Items) {
		return false
	}
	fn(&r.state.Items[idx])
	return true
}

// sameToken 报告 tok 是否仍是当前代次。
func (r *Runner) sameToken(tok runToken) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token == tok
}

func (r *Runner) stopPendingEmit() {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.pendingEmit != nil {
		r.pendingEmit.Stop()
		r.pendingEmit = nil
	}
}

func (r *Runner) updateIndex(tok runToken, idx int) {
	r.mu.Lock()
	if r.token != tok {
		r.mu.Unlock()
		return
	}
	r.state.Index = idx
	st := cloneState(r.state)
	r.mu.Unlock()
	r.stopPendingEmit()
	r.emitState(st)
}

// markRemaining 把 idx 之后的待处理项标为指定状态。
func (r *Runner) markRemaining(tok runToken, from int, status Status, msg string) {
	r.mu.Lock()
	if r.token != tok {
		r.mu.Unlock()
		return
	}
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
