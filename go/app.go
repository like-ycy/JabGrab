package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"jabgrab/internal/config"
	"jabgrab/internal/locate"
	"jabgrab/internal/pipeline"
	"jabgrab/internal/proc"
)

// LogLine 是推给前端的一行日志。
type LogLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// BootInfo 是前端启动时拿到的全部环境信息。
type BootInfo struct {
	Config     config.Config `json:"config"`
	ConfigPath string        `json:"configPath"`
	Issues     []string      `json:"issues"`
	DlrPath    string        `json:"dlrPath"`
	DlrSource  string        `json:"dlrSource"`
	DlrVersion string        `json:"dlrVersion"`
	ScrapeCmd  []string      `json:"scrapeCmd"`
	ScrapeSrc  string        `json:"scrapeSource"`
	ScrapeErr  string        `json:"scrapeErr"`
	FFmpegPath string        `json:"ffmpegPath"`
	FFmpegSrc  string        `json:"ffmpegSource"`
	// Components 是各外部组件的状态，与 ListComponents 同源；放一份在这里是
	// 为了让首屏一次请求就能画出设置页，不必再等一次往返。
	Components []ComponentDTO `json:"components"`
	// ComponentsDir 是已安装组件的落盘目录，出问题时用户要能直接找到它。
	ComponentsDir string         `json:"componentsDir"`
	State         pipeline.State `json:"state"`
}

// App 是绑定给前端的后端对象。
type App struct {
	ctx context.Context

	mu        sync.Mutex
	cfg       config.Config
	runner    *pipeline.Runner
	dlrVer    string
	dlrVerFor string

	// components 管理按需下载的外部组件（清单缓存、安装、卸载）。
	components *componentService
	// installMu 只保护下面两个 map，与 a.mu 分开：安装进度每 100ms 更新一次，
	// 混进 a.mu 会让「刷新状态」与「下载进度」互相阻塞。
	installMu       sync.Mutex
	installs        map[string]bool
	installProgress map[string][2]int64
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	cfg, loadErr := config.Load()
	if loadErr != nil {
		// 文件存在但读不出来（多半是手改出语法错误）：保留原文件让用户修，
		// 绝不能拿默认值覆盖掉他的保存路径、站点和下载参数。
		runtime.LogWarningf(ctx, "读取配置失败，本次使用默认值（磁盘上的原文件保持不变）: %v", loadErr)
	} else if !config.Exists() {
		// 仅首次运行才落盘默认配置，方便用户直接改文件
		saved, err := config.Save(cfg)
		if err != nil {
			runtime.LogWarningf(ctx, "写入默认配置失败: %v", err)
		}
		cfg = saved
	}

	a.components = newComponentService()
	a.cleanupComponents()

	a.mu.Lock()
	a.cfg = cfg
	a.runner = pipeline.New(cfg, pipeline.Hooks{
		OnState: func(s pipeline.State) {
			runtime.EventsEmit(ctx, "pipeline:state", s)
		},
		OnLog: func(level, text string) {
			runtime.EventsEmit(ctx, "pipeline:log", LogLine{Level: level, Text: text})
		},
		Resolve: a.resolveComponent,
	})
	a.mu.Unlock()
}

// emit 把一个事件推给前端。上下文未就绪时静默丢弃：组件安装是异步的，
// 窗口可能在下载途中就被关掉。
func (a *App) emit(event string, payload any) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, event, payload)
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner != nil {
		runner.Stop()
	}
}

// Boot 返回配置、环境自检结果与当前任务状态。
func (a *App) Boot() BootInfo {
	a.mu.Lock()
	cfg := a.cfg
	runner := a.runner
	a.mu.Unlock()

	info := BootInfo{Config: cfg}
	if p, err := config.Path(); err == nil {
		info.ConfigPath = p
	}
	info.Issues = cfg.Check()

	if p, src, err := locate.Dlr(cfg, a.candidates(locate.NameDlr)...); err == nil {
		info.DlrPath, info.DlrSource = p, src
		info.DlrVersion = a.dlrVersion(p)
	} else {
		info.Issues = append(info.Issues, err.Error())
	}

	if argv, _, src, err := locate.ScrapeSource(cfg, a.candidates(locate.NameScraper)...); err == nil {
		info.ScrapeCmd, info.ScrapeSrc = argv, src
	} else {
		info.ScrapeErr = err.Error()
		info.Issues = append(info.Issues, err.Error())
	}

	// ffmpeg 的「附近目录」取下载器所在目录：便携部署常把两者放在一起。
	var near []string
	if info.DlrPath != "" {
		near = append(near, filepath.Dir(info.DlrPath))
	}
	info.FFmpegPath, info.FFmpegSrc = locate.FFmpeg(cfg, near, a.candidates(locate.NameFFmpeg)...)

	if a.components.available() {
		info.ComponentsDir = a.components.rootDir()
	}
	info.Components = a.ListComponents()

	if runner != nil {
		info.State = runner.Snapshot()
	}
	return info
}

// SaveConfig 保存配置并返回最新的自检结果。
func (a *App) SaveConfig(cfg config.Config) (BootInfo, error) {
	cfg.PushRecentTxt(cfg.TxtPath)
	// Save 会补全默认值，必须用它的返回值更新运行态，否则用户清空某个字段后
	// 会出现「盘上已修正、内存里还是空」的错位，保存后当次会话反而起不来。
	cfg, err := config.Save(cfg)
	if err != nil {
		return BootInfo{}, err
	}

	a.mu.Lock()
	a.cfg = cfg
	runner := a.runner
	a.mu.Unlock()

	if runner != nil {
		runner.UpdateConfig(cfg)
	}
	return a.Boot(), nil
}

// SelectTxt 弹出文件选择框选 txt。
func (a *App) SelectTxt() (string, error) {
	a.mu.Lock()
	def := a.cfg.TxtPath
	a.mu.Unlock()

	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "选择待下载 URL 列表（txt）",
		DefaultDirectory: dirOf(def),
		Filters: []runtime.FileFilter{
			{DisplayName: "文本文件 (*.txt)", Pattern: "*.txt"},
		},
	})
	return path, err
}

// SelectDir 弹出目录选择框。
func (a *App) SelectDir() (string, error) {
	a.mu.Lock()
	def := a.cfg.SaveDir
	a.mu.Unlock()

	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                "选择视频保存目录",
		DefaultDirectory:     def,
		CanCreateDirectories: true,
	})
}

// SelectDlr 选择 N_m3u8DL-RE 可执行文件。
func (a *App) SelectDlr() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择 N_m3u8DL-RE 可执行文件",
	})
}

// Start 启动流水线，立即返回，进度通过事件推送。
func (a *App) Start() error {
	a.mu.Lock()
	runner := a.runner
	cfg := a.cfg
	a.mu.Unlock()

	if runner == nil {
		return errors.New("应用尚未初始化")
	}
	if runner.Running() {
		return errors.New("任务已在运行中")
	}
	if strings.TrimSpace(cfg.TxtPath) == "" {
		return errors.New("请先在设置里选择待下载 txt 文件")
	}
	if issues := cfg.Check(); len(issues) > 0 {
		return fmt.Errorf("配置有问题: %s", strings.Join(issues, "；"))
	}
	// 提前拦住：ffmpeg 缺失时 dl-re 启动即崩，等用户点开始后再逐条失败体验很差。
	// 定位链会查组件、随包、PATH 与「下载器同目录」（便携部署的常见摆法）。
	if dlr, _, err := locate.Dlr(cfg, a.candidates(locate.NameDlr)...); err == nil {
		if p, _ := locate.FFmpeg(cfg, []string{filepath.Dir(dlr)}, a.candidates(locate.NameFFmpeg)...); p == "" {
			return errors.New("未找到 ffmpeg：N_m3u8DL-RE 启动时必须校验 ffmpeg，" +
				"请先安装（macOS 可 brew install ffmpeg；Windows 把 ffmpeg.exe 所在目录加入 PATH，" +
				"或把 ffmpeg 放到下载器同目录）")
		}
	}

	go func() {
		if err := runner.Run(context.Background()); err != nil {
			runtime.EventsEmit(a.ctx, "pipeline:log", LogLine{
				Level: "error",
				Text:  err.Error(),
			})
		}
	}()
	return nil
}

// ClearDownloaded 把本轮已下好（完成 / 已存在）的 URL 从 txt 里删掉。
//
// 一次几十条里总掺着几条失败或没跑到，所以只删成功的那些：剩下的恰好就是
// 下一轮要重试的清单，再点「开始」只跑这几条。失败的、未处理的、注释与空行
// 全部原样留在文件里。
func (a *App) ClearDownloaded() (pipeline.PruneResult, error) {
	a.mu.Lock()
	runner := a.runner
	cfg := a.cfg
	a.mu.Unlock()

	if runner == nil {
		return pipeline.PruneResult{}, errors.New("应用尚未初始化")
	}
	// 运行中不动文件：状态还在往外推，删完列表与文件必然对不上，
	// 而且此时被删的行里可能正有一条马上要重试。
	if runner.Running() {
		return pipeline.PruneResult{}, errors.New("任务正在运行，请先停止再清理")
	}
	if strings.TrimSpace(cfg.TxtPath) == "" {
		return pipeline.PruneResult{}, errors.New("请先在设置里选择待下载 txt 文件")
	}

	var done []string
	for _, it := range runner.Snapshot().Items {
		if pipeline.Downloaded(it.Status) {
			done = append(done, it.URL)
		}
	}
	return pipeline.PruneURLs(cfg.TxtPath, done)
}

// Stop 停止当前任务（杀掉 dl-re / 爬取进程组）。
func (a *App) Stop() {
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner != nil {
		runner.Stop()
	}
}

// State 返回当前任务快照，供前端轮询兜底。
func (a *App) State() pipeline.State {
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner == nil {
		return pipeline.State{}
	}
	return runner.Snapshot()
}

// dlrVersion 取下载器版本（带缓存，避免每次刷新都拉一次进程）。
func (a *App) dlrVersion(bin string) string {
	a.mu.Lock()
	if a.dlrVerFor == bin && a.dlrVer != "" {
		v := a.dlrVer
		a.mu.Unlock()
		return v
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--version")
	proc.Prepare(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(v) > 120 {
		v = v[:120]
	}

	a.mu.Lock()
	a.dlrVer, a.dlrVerFor = v, bin
	a.mu.Unlock()
	return v
}

func dirOf(p string) string {
	if p == "" {
		return ""
	}
	i := strings.LastIndexAny(p, "/\\")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
