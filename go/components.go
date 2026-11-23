package main

// 按需下载的外部组件：清单缓存、状态查询、安装与卸载。
//
// 为什么要把 scraper 与 dlre 从安装包里拿出去
// ------------------------------------------
// 只改 Go 代码发一次版，用户原本要重下 30MB，其中 26MB 是半年不动的 CPython
// 运行时和上游下载器。拆开之后 App 包 3.5MB，组件各自带自己的版本号，只在
// 自己变了的时候发新版。设计见 docs/release-split-plan.md。
//
// 为什么单独一层而不是都塞进 App
// ------------------------------
// 清单缓存、安装互斥、后台刷新这三件事与 App 的其他状态无关，混在一起后
// a.mu 会被网络请求拖住（一次清单请求 25 秒，期间界面刷新全部阻塞）。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"jabgrab/internal/component"
	"jabgrab/internal/config"
	"jabgrab/internal/locate"
)

// 推给前端的组件事件。与 pipeline:* 一样是对外契约。
const (
	eventComponentProgress  = "component:progress"
	eventComponentDone      = "component:done"
	eventComponentFailed    = "component:failed"
	eventComponentRefreshed = "component:refreshed"
)

// componentManifestURL 指向最近一次 Release 附带的组件清单。
//
// 用 /releases/latest/download/ 而不是锚定某个版本：组件有自己的版本号，锚定
// App 版本会让「爬取规则修复」必须跟着发一版 App。协议不匹配时会退回内置清单，
// 所以拿到更新的清单不会让老 App 装上不兼容的组件。
//
// 可用 JABGRAB_COMPONENT_MANIFEST_URL 覆盖（自建发布源、本地联调）。
// 覆盖只改「清单从哪来」：包本身仍按清单里的 sha256 校验，改不了装什么。
var componentManifestURL = "https://github.com/like-ycy/JabGrab/releases/latest/download/manifest.json"

func init() {
	if v := strings.TrimSpace(os.Getenv("JABGRAB_COMPONENT_MANIFEST_URL")); v != "" {
		componentManifestURL = v
	}
}

// manifestTTL 是在线清单的复用窗口。
//
// 组件更新是低频事件，而设置页每次进入都会问一次状态。窗口内直接用上次的
// 结果，避免每次翻设置页都打一次 GitHub。
const manifestTTL = 6 * time.Hour

// componentDef 描述一个可下载组件在 App 侧的语义。
type componentDef struct {
	Name   string
	Label  string
	Binary string
	// Protocol 是组件与 App 的接口版本，见 docs/release-split-plan.md §7。
	Protocol int
	// Optional 为真的组件缺失时只降级，不拦流程。当前两个都缺一不可，
	// 留这个字段是给将来的 ffmpeg 组件（机制已就绪，暂不发布资产）。
	Optional bool
}

// componentDefs 是当前支持按需安装的组件。顺序即界面顺序。
//
// 协议取 1：
//   - scraper：argv `<url> --source <jable|missav>` + stdout 单行 JSON
//   - dlre：argv 参数集 + stdout 进度行格式（downloader 靠它算百分比）
//
// 两处都是「删字段/改语义才 bump，只增不 bump」。
func componentDefs() []componentDef {
	return []componentDef{
		{Name: locate.NameScraper, Label: "爬取程序", Binary: "jab-scrape", Protocol: 1},
		{Name: locate.NameDlr, Label: "下载器", Binary: "N_m3u8DL-RE", Protocol: 1},
	}
}

func findComponentDef(name string) (componentDef, bool) {
	for _, def := range componentDefs() {
		if def.Name == name {
			return def, true
		}
	}
	return componentDef{}, false
}

// ComponentDTO 是一个组件的当前状态，供设置页与任务页展示。
type ComponentDTO struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// State：missing/outdated/ready/corrupt/incompatible，见 component.State。
	State            string `json:"state"`
	InstalledVersion string `json:"installedVersion"`
	AvailableVersion string `json:"availableVersion"`
	Protocol         int    `json:"protocol"`
	// Size 是待下载包的字节数；0 表示清单里没有（还没拿到可用清单）。
	Size int64 `json:"size"`
	// Source 是当前实际生效的来源（配置指定/组件/随包/程序同级目录/开发目录/PATH），
	// 空表示没有可用的可执行文件。
	Source string `json:"source"`
	Path   string `json:"path"`
	// CanInstall 表示「现在点下载能成功」，依赖清单里是否有本平台的包。
	CanInstall bool `json:"canInstall"`
	// OnDemand 是用户的开关状态；关掉时界面只展示状态、不提供下载入口。
	OnDemand bool `json:"onDemand"`
	Optional bool `json:"optional"`
	// Installing 表示此刻有安装任务在跑。前端切走页面再回来时要靠它恢复
	// 「正在下载」的按钮状态 —— 只靠后续事件的话，重挂载后的界面看起来像什么
	// 都没发生，用户会再点一次下载。
	Installing    bool  `json:"installing"`
	ProgressDone  int64 `json:"progressDone"`
	ProgressTotal int64 `json:"progressTotal"`
	// Message 是给人看的一句话，说明为什么不能装 / 为什么不可用。
	Message string `json:"message"`
}

// componentService 管理按需组件：清单缓存、状态查询、安装。
type componentService struct {
	mu      sync.Mutex
	manager *component.Manager
	err     error

	client *http.Client

	manifest    component.Manifest
	manifestAt  time.Time
	manifestOK  bool
	attemptedAt time.Time
}

func newComponentService() *componentService {
	dir, err := component.DefaultDir()
	if err != nil {
		return &componentService{
			client: component.NewHTTPClient(20 * time.Second),
			err:    err,
		}
	}
	return newComponentServiceAt(dir)
}

// newComponentServiceAt 在指定目录上构造服务（测试用；产品侧走 DefaultDir）。
func newComponentServiceAt(dir string) *componentService {
	return &componentService{
		client:  component.NewHTTPClient(20 * time.Second),
		manager: component.New(dir),
	}
}

func (s *componentService) available() bool { return s != nil && s.manager != nil }

// path 返回已安装组件的可执行文件路径，没有则空。
func (s *componentService) path(name string) string {
	if !s.available() {
		return ""
	}
	return s.manager.Path(name)
}

// rootDir 返回组件清单的本地来源，供环境诊断展示。
func (s *componentService) rootDir() string {
	if !s.available() {
		return ""
	}
	return s.manager.Root()
}

// cachedManifest 返回 TTL 内的在线清单。
func (s *componentService) cachedManifest() (component.Manifest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.manifestOK || time.Since(s.manifestAt) > manifestTTL {
		return component.Manifest{}, false
	}
	return s.manifest, true
}

// refreshManifest 拉取并缓存在线清单。
func (s *componentService) refreshManifest(ctx context.Context, mirror string) (component.Manifest, error) {
	manifest, err := component.FetchManifest(ctx, s.client, componentManifestURL, mirror)
	if err != nil {
		return component.Manifest{}, err
	}
	s.mu.Lock()
	s.manifest, s.manifestAt, s.manifestOK = manifest, time.Now(), true
	s.mu.Unlock()
	return manifest, nil
}

// refreshAsync 在后台刷新在线清单，失败只记日志。
//
// 失败不弹窗：设置页只是想知道「有没有可更新的组件」，为此打断用户不合理。
// 真正需要下载时会再拉一次，并把错误摆在下载按钮旁边。
func (s *componentService) refreshAsync(mirror string, onRefreshed func()) {
	// 最小间隔：断网时每次切页面都会走到这里，没有这个门槛就是每次导航打一次
	// GitHub（并在 25 秒后超时），既无意义也让日志刷屏。
	s.mu.Lock()
	if time.Since(s.attemptedAt) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.attemptedAt = time.Now()
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if _, err := s.refreshManifest(ctx, mirror); err != nil {
			log.Printf("提示：组件清单暂不可用（%v）", err)
			return
		}
		onRefreshed()
	}()
}

// specLocal 返回不联网就能得到的安装描述。
//
// 优先在线清单（可能比内置的新），协议不匹配则退回内置：清单来自更晚的
// Release 时，它的组件可能是给新协议准备的，装上去也只会报「协议不匹配」。
// 内置清单由同一次构建生成，协议必然一致。
func (s *componentService) specLocal(def componentDef) (component.Spec, string, error) {
	var problems []string
	if manifest, ok := s.cachedManifest(); ok {
		if spec, err := manifest.SpecForCurrent(def.Name); err == nil {
			if spec.Protocol == def.Protocol {
				return spec, "manifest", nil
			}
			problems = append(problems, fmt.Sprintf("在线清单的协议为 v%d（本程序需要 v%d）", spec.Protocol, def.Protocol))
		} else {
			problems = append(problems, err.Error())
		}
	}
	if builtin, err := component.BuiltinManifest(); err == nil {
		if spec, err := builtin.SpecForCurrent(def.Name); err == nil {
			if spec.Protocol == def.Protocol {
				return spec, "builtin", nil
			}
			problems = append(problems, fmt.Sprintf("内置清单的协议为 v%d（本程序需要 v%d）", spec.Protocol, def.Protocol))
		} else {
			problems = append(problems, err.Error())
		}
	} else {
		problems = append(problems, "本构建未内置组件清单")
	}
	return component.Spec{}, "", fmt.Errorf("无法确定%s的下载信息：%s", def.Label, strings.Join(problems, "；"))
}

// specForInstall 返回用于安装的描述：先拉最新的在线清单，失败退回本地。
//
// 安装是显式操作，多等几秒换「装上最新的组件」是值得的；但网络不通时不能
// 连装都不让装 —— 内置清单里的包同样能装。
func (s *componentService) specForInstall(ctx context.Context, def componentDef, mirror string) (component.Spec, error) {
	if manifest, err := s.refreshManifest(ctx, mirror); err == nil {
		if spec, err := manifest.SpecForCurrent(def.Name); err == nil && spec.Protocol == def.Protocol {
			return withMirror(spec, mirror), nil
		}
	} else {
		log.Printf("提示：使用内置组件清单（在线清单不可用：%v）", err)
	}
	spec, _, err := s.specLocal(def)
	if err != nil {
		return component.Spec{}, err
	}
	return withMirror(spec, mirror), nil
}

// withMirror 把镜像前缀应用到包下载地址。
//
// 清单里的地址是 GitHub 原始地址，镜像只加在「清单请求」上是不够的：用户填了
// 镜像，清单能取到、包却仍然直连 GitHub，下载依旧卡住 —— 而界面一切正常，
// 只是永远停在 0%。
func withMirror(spec component.Spec, mirror string) component.Spec {
	if mirror == "" {
		return spec
	}
	spec.Asset.URL = spec.Asset.URLFor(mirror)
	return spec
}

// ── App 上的绑定方法 ──────────────────────────────────────────────────────

// ListComponents 返回所有可下载组件的状态。
//
// 状态查询不联网（用缓存或内置清单），同时按需在后台刷新：设置页要的是立刻能
// 画出来的东西，而不是等 20 秒的 GitHub 响应。
func (a *App) ListComponents() []ComponentDTO {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()

	// 返回空切片而不是 nil：Wails 把 nil 序列化成 null，前端 `.map` 会炸。
	out := make([]ComponentDTO, 0, len(componentDefs()))

	svc := a.components
	// 组件目录不可用（配置目录建不出来）时也要给出每个组件的条目：
	// 一张写着「组件目录不可用」的卡片，比整个区块消失更好排查。
	if svc.available() && !hasManifest(svc) {
		svc.refreshAsync(cfg.ComponentMirror, func() { a.emit(eventComponentRefreshed, struct{}{}) })
	}
	for _, def := range componentDefs() {
		out = append(out, a.componentStatus(def, cfg))
	}
	return out
}

func hasManifest(svc *componentService) bool {
	_, ok := svc.cachedManifest()
	return ok
}

func (a *App) componentStatus(def componentDef, cfg config.Config) ComponentDTO {
	dto := ComponentDTO{
		Name:     def.Name,
		Label:    def.Label,
		Protocol: def.Protocol,
		OnDemand: cfg.ComponentsOnDemandEnabled(),
		Optional: def.Optional,
	}
	svc := a.components
	if !svc.available() {
		dto.Message = "组件目录不可用，请检查配置目录权限"
		return dto
	}

	spec, _, err := svc.specLocal(def)
	if err != nil {
		dto.Message = err.Error()
	} else {
		dto.AvailableVersion = spec.Version
		dto.Size = spec.Asset.Size
		dto.CanInstall = true
	}

	if version, _, ok := svc.manager.Installed(def.Name); ok {
		dto.InstalledVersion = version
	}
	// 拿不到清单时只判可用性、不判版本：这样已装的组件仍会显示
	// ready/corrupt/incompatible，而不是因为「期望版本未知」被标成可更新。
	if spec.Name == "" {
		dto.State = svc.manager.LocalState(def.Name, def.Protocol).String()
	} else {
		dto.State = svc.manager.State(spec).String()
	}
	dto.Path, dto.Source = a.componentInUse(def.Name)
	dto.Installing, dto.ProgressDone, dto.ProgressTotal = a.installSnapshot(def.Name)
	if dto.Path == "" && dto.State == component.Corrupt.String() {
		dto.Message = "组件文件不完整，重新下载即可修复"
	}
	return dto
}

// installSnapshot 返回某个组件的安装快照（是否在装、装到哪了）。
func (a *App) installSnapshot(name string) (bool, int64, int64) {
	a.installMu.Lock()
	defer a.installMu.Unlock()
	if !a.installs[name] {
		return false, 0, 0
	}
	p := a.installProgress[name]
	return true, p[0], p[1]
}

// componentInUse 返回某个组件当前实际生效的路径与来源。
func (a *App) componentInUse(name string) (string, string) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()

	switch name {
	case locate.NameScraper:
		if argv, _, src, err := locate.ScrapeSource(cfg, a.candidates(name)...); err == nil && len(argv) > 0 {
			return argv[0], src
		}
	case locate.NameDlr:
		if p, src, err := locate.Dlr(cfg, a.candidates(name)...); err == nil {
			return p, src
		}
	}
	return "", ""
}

// InstallComponent 启动一次组件安装，立即返回。
//
// 安装包含几十 MB 下载与解压，同步等待会让前端请求挂几十秒，期间界面连进度
// 都画不出来。进度走 component:progress 事件，结束走 done/failed。
func (a *App) InstallComponent(name string) error {
	def, ok := findComponentDef(name)
	if !ok {
		return fmt.Errorf("未知组件：%s", name)
	}
	svc := a.components
	if !svc.available() {
		return errors.New("组件目录不可用，请检查配置目录权限")
	}
	if a.ctx == nil {
		return errors.New("应用尚未就绪")
	}
	// 任务运行中不许换组件：Windows 上正在跑的 jab-scrape.exe 不能被替换，
	// macOS 上虽然能换，但这次任务的输出与刚装上的版本对不上，出了问题会
	// 归因到错误的版本上。
	if a.runner != nil && a.runner.Running() {
		return errors.New("任务正在运行，请先停止再下载组件")
	}

	a.mu.Lock()
	mirror := a.cfg.ComponentMirror
	a.mu.Unlock()

	a.setInstalling(name, true)
	go func() {
		defer a.setInstalling(name, false)

		// 下载在慢网下要几分钟，解压还要几秒。整体超时给足，具体取消由 ctx
		// 承担：用户关窗口时 a.ctx 会被取消，点「取消」时走 installCancel。
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
		defer cancel()
		a.installMu.Lock()
		if a.installCancel == nil {
			a.installCancel = map[string]context.CancelFunc{}
		}
		a.installCancel[name] = cancel
		a.installMu.Unlock()
		defer func() {
			a.installMu.Lock()
			delete(a.installCancel, name)
			a.installMu.Unlock()
		}()

		spec, err := svc.specForInstall(ctx, def, mirror)
		if err != nil {
			a.emit(eventComponentFailed, componentFailedDTO{Name: name, Message: err.Error()})
			return
		}

		var lastEmit time.Time
		progress := func(done, total int64) {
			// 限流：下载器每读一块就回调一次，原样转发会把 WebView 的事件队列
			// 塞满，界面反而更卡。末尾那一次必须发出，否则进度条会停在 99%。
			if done < total && time.Since(lastEmit) < 100*time.Millisecond {
				return
			}
			lastEmit = time.Now()
			a.recordProgress(name, done, total)
			a.emit(eventComponentProgress, componentProgressDTO{Name: name, Done: done, Total: total})
		}

		path, err := svc.manager.Install(ctx, spec, progress)
		if err != nil {
			a.emit(eventComponentFailed, componentFailedDTO{Name: name, Message: friendlyInstallError(err)})
			return
		}
		log.Printf("组件 %s %s 已安装：%s", def.Name, spec.Version, path)
		a.emit(eventComponentDone, componentDoneDTO{Name: name, Version: spec.Version, Path: path})
	}()
	return nil
}

// RemoveComponent 卸载按需安装的组件。
//
// 只删组件目录，不动用户手工配置的路径与随包副本：那两者不归这里管，删了之后
// 定位链会自然回落到它们。
func (a *App) RemoveComponent(name string) error {
	def, ok := findComponentDef(name)
	if !ok {
		return fmt.Errorf("未知组件：%s", name)
	}
	svc := a.components
	if !svc.available() {
		return errors.New("组件目录不可用")
	}
	if a.runner != nil && a.runner.Running() {
		return errors.New("任务正在运行，请先停止再卸载组件")
	}
	a.setInstalling(name, true)
	defer a.setInstalling(name, false)
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return svc.manager.Remove(ctx, def.Name)
}

// CancelInstall 取消正在进行的组件安装（安装卡片上的「取消」按钮）。
func (a *App) CancelInstall(name string) error {
	a.installMu.Lock()
	cancel := a.installCancel[name]
	a.installMu.Unlock()
	if cancel == nil {
		return fmt.Errorf("组件 %s 当前没有安装任务", name)
	}
	cancel()
	return nil
}

func (a *App) setInstalling(name string, on bool) {
	a.installMu.Lock()
	defer a.installMu.Unlock()
	if a.installs == nil {
		a.installs = map[string]bool{}
	}
	if on {
		a.installs[name] = true
		return
	}
	delete(a.installs, name)
	delete(a.installProgress, name)
}

func (a *App) recordProgress(name string, done, total int64) {
	a.installMu.Lock()
	defer a.installMu.Unlock()
	if a.installProgress == nil {
		a.installProgress = map[string][2]int64{}
	}
	a.installProgress[name] = [2]int64{done, total}
}

// candidates 返回某个组件「已安装」的候选路径，喂给 locate 的定位链。
func (a *App) candidates(name string) []locate.Candidate {
	if !a.components.available() {
		return nil
	}
	if p := a.components.path(name); p != "" {
		return []locate.Candidate{{Path: p, Source: locate.SourceComponent}}
	}
	return nil
}

// resolveComponent 是喂给 pipeline 的 Resolver。
func (a *App) resolveComponent(name string) []locate.Candidate { return a.candidates(name) }

// cleanupComponents 清理上次留下的半成品与旧版本目录。
//
// 启动时在后台跑：失败只影响磁盘占用，不该拦住应用启动。
func (a *App) cleanupComponents() {
	svc := a.components
	if !svc.available() {
		return
	}
	go func() {
		ctx := a.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := svc.manager.CleanupStale(ctx); err != nil {
			log.Printf("提示：清理旧组件目录失败（下次启动会再试）：%v", err)
		}
	}()
}

type componentProgressDTO struct {
	Name  string `json:"name"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

type componentDoneDTO struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

type componentFailedDTO struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// friendlyInstallError 把底层错误换成用户能行动的说法。
//
// 安装失败最常见的两种是「下不动」与「校验不符」。原样抛出 net/http 的错误串，
// 用户只能看到一堆英文，既不知道能不能重试，也不知道该改什么。
func friendlyInstallError(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "下载超时。可换用镜像后重试（设置 → 外部组件 → 组件下载源）。"
	case errors.Is(err, context.Canceled):
		return "下载已取消。"
	case strings.Contains(msg, "校验"):
		return "下载的组件包校验不符，可能被网络中间件改写。请重试，或换用镜像。"
	case strings.Contains(msg, "空间不足"):
		return msg + "。清理磁盘后重试，或改用带组件的整包。"
	}
	return msg
}
