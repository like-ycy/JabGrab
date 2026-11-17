// Package component 管理「按需下载的外部组件」。
//
// 为什么需要这一层
// ----------------
// scraper 占分发目录的 65%（31MB / 48MB），其中 96% 是 CPython 运行时。
// 绝大多数用户第一次打开 App 只是看视频，刮削是后来才用到的能力 —— 让所有人
// 先下 20MB 的运行时并不划算，见 docs/size-reduction-plan.md §3。
//
// 负责什么、不负责什么
// --------------------
//   - 只做「拿到文件、校验、放到该在的位置」，不认识 scraper 的协议与参数：
//     叫它 scraper 还是 ffprobe 只是 Spec.Name 不同。
//   - 不做版本协商。期望版本由调用方传入（来自 manifest，或 App 内置的兜底表）。
//     这样断网时也能告诉用户「需要哪个版本」，而不是卡在「未知」。
//   - 不碰 UI。进度走回调，怎么画是前端的事。
//
// 磁盘布局
// --------
//
//	<data>/components/
//	├── installed.json          # 指针：当前生效的版本目录
//	├── scraper-0.4.3/          # 每个版本一个目录，见 install.go 的注释
//	│   ├── .component.json     # 安装凭据：版本、协议、包 sha256、文件清单
//	│   ├── scraper
//	│   └── _internal/
//	└── .staging-1234567/       # 半成品，只存在于安装过程中
package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"jabgrab/internal/config"
)

const (
	// pointerFile 记录当前生效的版本目录。
	//
	// 为什么要有指针文件，而不是「扫目录找最大的版本号」：手动放回的整包、
	// 装到一半失败的残留、同名不同来源的副本都可能同时存在，靠版本号排序
	// 猜哪个该用，猜错的表现是「刮削器版本莫名变旧」，而现场看不出任何线索。
	pointerFile = "installed.json"

	// receiptFile 放在每个版本目录内，记录这个目录是谁装的、装的是什么。
	// 它与目录同生共死：目录被删掉，凭据也一起没了，不会留下「记录说装了、
	// 实际文件不在」的分裂状态。
	receiptFile = "component.json"

	// stagingPrefix 是半成品目录的前缀。用点开头，避免与真实组件目录混淆。
	stagingPrefix = ".staging-"

	pointerSchema = 1
)

// State 是组件在本机的实际状态。
type State int

const (
	// Missing 表示从未安装。
	Missing State = iota
	// Outdated 表示装了可用版本，但与期望版本不同 —— 可以更新。
	Outdated
	// Ready 表示版本与协议都与期望一致，文件齐全。
	Ready
	// Corrupt 表示目录或可执行文件缺失、凭据不可读。文件被删了一半、
	// 下载被中断、杀软隔离了某个 dll，都落到这一档。
	Corrupt
	// Incompatible 表示协议版本对不上。这一档与 Outdated 分开：
	// 协议不匹配时升级组件也未必能用，得先升级 App，用户需要的是不同的引导。
	Incompatible
)

func (s State) String() string {
	switch s {
	case Missing:
		return "missing"
	case Outdated:
		return "outdated"
	case Ready:
		return "ready"
	case Corrupt:
		return "corrupt"
	case Incompatible:
		return "incompatible"
	default:
		return "unknown"
	}
}

// Asset 是一份可下载的组件包。
type Asset struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Spec 描述「我想要哪个组件、什么版本、从哪里下」。
type Spec struct {
	// Name 是组件名，同时是落盘目录的前缀（"scraper" → "scraper-0.4.3"）。
	Name string
	// Version 是组件自身的版本号，与 App 版本无关。
	Version string
	// Protocol 是组件与 App 之间的接口版本（scraper 对应 NDJSON 协议版本）。
	Protocol int
	// Binary 是目录内可执行文件名，不含后缀。为空时取 Name。
	Binary string
	// Asset 是下载地址与校验信息。
	Asset Asset
}

// BinaryName 返回可执行文件名（带平台后缀）。
func (s Spec) BinaryName() string {
	return exeName(s.binaryBase())
}

func (s Spec) binaryBase() string {
	if s.Binary != "" {
		return s.Binary
	}
	return s.Name
}

// Validate 检查 Spec 是否可用于安装。
//
// 单列一个方法而不是散在 Install 里，是因为这些字段都可能来自网络上的
// manifest：Name 会拼进文件路径，校验不过就是目录穿越。
func (s Spec) Validate() error {
	if err := safeSegment("组件名", s.Name); err != nil {
		return err
	}
	if err := safeSegment("版本号", s.Version); err != nil {
		return err
	}
	if s.Binary != "" {
		if err := safeSegment("可执行文件名", s.Binary); err != nil {
			return err
		}
	}
	if s.Asset.URL == "" {
		return errors.New("组件下载地址为空")
	}
	if err := ValidateURL(s.Asset.URL); err != nil {
		return err
	}
	if err := validateSHA256(s.Asset.SHA256); err != nil {
		return err
	}
	if s.Asset.Size < 0 {
		return fmt.Errorf("组件包大小非法：%d", s.Asset.Size)
	}
	return nil
}

// ValidateURL 要求下载地址是 https，本机回环允许 http。
//
// 为什么必须 https
// ----------------
// 清单里的 sha256 与包本身来自同一个源（都是这个 URL 指向的目录）。纯 http
// 下，能改写响应的一方可以同时替换包与校验码，于是「校验通过」什么也没证明，
// 装进去的是任意可执行文件，而 App 会在环境自检时直接运行它。
//
// 回环地址放行：本地联调与测试跑 httptest 用 http，它不经过网络，没有中间人。
func ValidateURL(raw string) error {
	if raw == "" {
		return errors.New("地址为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("地址无法解析：%s", raw)
	}
	// 没有主机名的地址（例如只填了 "https://"）在拼上文件名后依然能通过
	// 协议检查，但请求必然失败。早一点说清楚，比下载时报「invalid URL」好。
	if parsed.Host == "" {
		return fmt.Errorf("地址缺少主机名：%s", raw)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(parsed.Hostname()) {
			return nil
		}
		return fmt.Errorf("下载地址必须用 https（http 只允许本机回环）：%s", raw)
	default:
		return fmt.Errorf("下载地址只支持 http(s)：%s", raw)
	}
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// safeSegment 校验会拼进文件路径的字段。
func safeSegment(what, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%s为空", what)
	case value == "." || value == "..":
		return fmt.Errorf("%s非法：%q", what, value)
	case strings.ContainsAny(value, `/\`):
		return fmt.Errorf("%s不能包含路径分隔符：%q", what, value)
	case strings.ContainsRune(value, 0):
		return fmt.Errorf("%s不能包含空字节", what)
	}
	return nil
}

// ProgressFunc 报告下载进度。total 为 0 表示服务端没给长度。
type ProgressFunc func(done, total int64)

// Manager 管理一个组件根目录下的所有组件。
//
// 并发：同一进程内的 Install/Remove/CleanupStale/Verify 互斥；跨进程靠文件锁
// （见 lock.go），因为两个 App 实例同时安装会互相删对方的 staging 目录。
type Manager struct {
	root   string
	client *http.Client
	// sem 是容量 1 的信号量，代替 sync.Mutex。
	//
	// 为什么不用互斥锁：一次安装可能在下载阶段停留十几分钟（20MB 慢网）。
	// 互斥锁上等待的第二个调用既不能取消也不能超时，用户点了「取消」之后
	// 界面毫无反应，直到前一次下载自然结束 —— 那比直接告诉他「正在安装」更糟。
	sem chan struct{}
}

// DefaultDir 返回默认的组件根目录。
//
// 与配置、索引库放在同一个数据目录下：跨盘 rename 在 Windows 上会直接失败，
// 而安装的最后一步正是 rename。
func DefaultDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "components"), nil
}

// New 创建一个 Manager。root 不存在时由 Install 按需创建。
func New(root string) *Manager {
	// 先把 root 解析成真实路径，后续所有操作都在真实路径空间里进行。
	//
	// 两个理由，都是真实踩到的：
	//   - root 本身是符号链接时 os.MkdirAll 会失败（它对最后一段用 Lstat，
	//     看到的是 symlink 而不是目录，于是尝试 mkdir，得到 EEXIST）。
	//     macOS 的 /var、/tmp，Linux 上常见的 /home → /data/home 都在这个范围里。
	//   - 解压时判断「符号链接落点是否在目录内」要拿 EvalSymlinks 的结果比，
	//     若 root 还是别名就永远比不上，合法链接会被判成越界。
	//
	// root 不存在（首次安装）时解析会失败，保持原样即可。
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return &Manager{
		root: root,
		sem:  make(chan struct{}, 1),
		// 20MB 的包在慢网下要几分钟，所以整体超时给足；进度与取消由调用方
		// 通过 ctx 控制。重定向校验见 NewHTTPClient 的说明。
		client: NewHTTPClient(15 * time.Minute),
	}
}

// lock 获取进程内互斥，ctx 可取消。
func (m *Manager) lock(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("另一个组件操作正在进行: %w", ctx.Err())
	}
}

func (m *Manager) unlock() {
	<-m.sem
}

// Root 返回组件根目录。
func (m *Manager) Root() string { return m.root }

// Path 返回已安装组件的可执行文件路径；没装或不可用时返回空字符串。
//
// 刻意不返回 error，与 toolpath.Find 的理由相同：调用方对「没有」的处理
// 各不相同，统一成 error 只会让每处都多写一遍判断。
//
// 「可用」包含可执行位：权限被改回 0644（备份还原、杀软、解压工具都可能这么干）
// 时路径仍然存在，但 exec 会 permission denied。返回非空路径等于把「跑不起来」
// 伪装成「装好了」，而调用方一般不会再查一遍。
func (m *Manager) Path(name string) string {
	entry, ok := m.lookup(name)
	if !ok {
		return ""
	}
	dir := filepath.Join(m.root, entry.Dir)
	rc, err := readReceipt(dir)
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, exeName(baseName(rc.Binary, name)))
	if !isExecutableFile(path) {
		return ""
	}
	return path
}

// Installed 返回已装组件的版本与协议，不判断是否可用。
//
// State 关心的是「能不能用」，界面还需要「装的是哪一版」才能说清
// 「已装 0.4.2，可更新到 0.4.3」。两者分开，避免 State 为了多返回一个字段
// 而承担展示语义。
func (m *Manager) Installed(name string) (version string, protocol int, ok bool) {
	entry, ok := m.lookup(name)
	if !ok {
		return "", 0, false
	}
	dir := filepath.Join(m.root, entry.Dir)
	rc, err := readReceipt(dir)
	if err != nil {
		// 指针在但凭据读不出来：目录被删或写坏。仍然算「装过」——
		// 否则界面会说「未安装」，而磁盘上留着几十 MB 垃圾。
		return entry.Version, entry.Protocol, true
	}
	return rc.Version, rc.Protocol, true
}

// State 返回组件状态（相对于「期望的版本与协议」）。
func (m *Manager) State(spec Spec) State {
	state := m.LocalState(spec.Name, spec.Protocol)
	if state != Ready {
		return state
	}
	if entry, ok := m.lookup(spec.Name); ok && entry.Version != spec.Version {
		return Outdated
	}
	return Ready
}

// LocalState 判断已装组件是否可用，不比较版本。
//
// 需要一个「没有期望版本」的判定：离线且构建没有内置清单时，调用方拿不到
// 该装哪个版本，硬拿空版本去比会得到一个恒定成立的 Outdated —— 界面会一直
// 显示「可更新」，点更新又什么都没有。可用性判断（协议、文件、执行位）
// 不依赖期望版本，这部分单独成立。
func (m *Manager) LocalState(name string, protocol int) State {
	entry, ok := m.lookup(name)
	if !ok {
		return Missing
	}
	dir := filepath.Join(m.root, entry.Dir)
	rc, err := readReceipt(dir)
	if err != nil {
		// 指针在、凭据读不出来：目录被删了或写坏了。这一档不能当成 Missing，
		// 否则 UI 会显示「未安装」，而磁盘上还留着几十 MB 垃圾。
		return Corrupt
	}
	// 协议先于版本判断：协议不匹配的组件装上也不能用，
	// 提示「去更新组件」会把用户引到错误的方向。
	if rc.Protocol != protocol {
		return Incompatible
	}
	if !isExecutableFile(filepath.Join(dir, exeName(baseName(rc.Binary, name)))) {
		return Corrupt
	}
	return Ready
}

// ── 指针与凭据 ────────────────────────────────────────────────────────────

type pointerEntry struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	Dir      string `json:"dir"`
}

type pointer struct {
	Schema     int                     `json:"schema"`
	Components map[string]pointerEntry `json:"components"`
}

type fileEntry struct {
	Path string `json:"path"`
	// Size 是文件字节数；符号链接为 0（链接本身不占内容）。
	Size int64 `json:"size"`
	// Link 非空表示这是一条符号链接，值是链接目标。
	// 单列字段而不是「只有 Path」，是因为验证方式完全不同：
	// 文件比大小，链接比目标 —— 用 os.Stat 查链接会跟到目标上，
	// 目标在就报「正常」，链接被换掉也发现不了。
	Link string `json:"link,omitempty"`
}

type receipt struct {
	Name          string      `json:"name"`
	Version       string      `json:"version"`
	Protocol      int         `json:"protocol"`
	Binary        string      `json:"binary"`
	ArchiveSHA256 string      `json:"archive_sha256"`
	InstalledAt   time.Time   `json:"installed_at"`
	Files         []fileEntry `json:"files"`
}

func (m *Manager) lookup(name string) (pointerEntry, bool) {
	p, err := readPointer(m.root)
	if err != nil {
		return pointerEntry{}, false
	}
	entry, ok := p.Components[name]
	if !ok || entry.Dir == "" {
		return pointerEntry{}, false
	}
	// 指针里的 Dir 会拼进路径，读回来也要校验一遍：这是磁盘上的文件，
	// 手改或损坏都可能让它指向任意位置。
	if err := safeSegment("组件目录", entry.Dir); err != nil {
		return pointerEntry{}, false
	}
	return entry, true
}

func readPointer(root string) (pointer, error) {
	data, err := os.ReadFile(filepath.Join(root, pointerFile))
	if err != nil {
		return pointer{}, err
	}
	var p pointer
	if err := json.Unmarshal(data, &p); err != nil {
		return pointer{}, fmt.Errorf("解析 %s: %w", pointerFile, err)
	}
	if p.Components == nil {
		p.Components = map[string]pointerEntry{}
	}
	return p, nil
}

// writePointer 原子写入指针。
func writePointer(root string, p pointer) error {
	p.Schema = pointerSchema
	if p.Components == nil {
		p.Components = map[string]pointerEntry{}
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s: %w", pointerFile, err)
	}
	path := filepath.Join(root, pointerFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s: %w", path, err)
	}
	return nil
}

// upsertPointer 把某个组件的指针改到指定目录。
func (m *Manager) upsertPointer(name string, entry pointerEntry) error {
	p, err := readPointer(m.root)
	if err != nil {
		// 指针不存在或损坏都不该挡住安装：损坏的那份本来就该被覆盖。
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("警告：组件指针不可读（%v），将重建", err)
		}
		p = pointer{Schema: pointerSchema, Components: map[string]pointerEntry{}}
	}
	if p.Components == nil {
		p.Components = map[string]pointerEntry{}
	}
	p.Components[name] = entry
	return writePointer(m.root, p)
}

func readReceipt(dir string) (receipt, error) {
	data, err := os.ReadFile(filepath.Join(dir, receiptFile))
	if err != nil {
		return receipt{}, err
	}
	var rc receipt
	if err := json.Unmarshal(data, &rc); err != nil {
		return receipt{}, fmt.Errorf("解析 %s: %w", receiptFile, err)
	}
	return rc, nil
}

// ── 小工具 ────────────────────────────────────────────────────────────────

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func baseName(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// isExecutableFile 判断路径是不是一个「能跑起来」的文件。
//
// macOS/Linux 上只看「是普通文件」不够：安装时权限被改成 0644（下载工具、
// 备份还原、某些杀软都会这么干）时 exec 会直接 permission denied，而
// State 仍报 Ready —— 用户点重装也没用，因为幂等分支直接返回了。
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return hasExecBit(info)
}

// hasExecBit 在 Windows 上恒为真：可执行性由扩展名决定，没有权限位这一说。
func hasExecBit(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}
