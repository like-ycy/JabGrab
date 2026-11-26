// Package locate 负责在运行时定位外部可执行文件。
//
// 三种工具（jab-scrape / N_m3u8DL-RE / ffmpeg）走同一条链，顺序即契约：
//
//  1. 配置里写死的路径（用户显式指定，错了就报错，不静默回退）
//  2. 调用方注入的候选 —— 按需下载的组件（<config>/components/<name>-<ver>/）
//  3. 随包 tools/<name>/（整包解压后的布局，见 tools/build.sh）
//  4. 旧扁平布局：主程序同级目录、同级 bin/、同级 python/（已发布的 zip 就这样）
//  5. 开发布局：third_party/、工作目录及其上级（go run / 本地构建）
//  6. 系统 PATH
//
// 为什么组件（2）排在随包（3）之前：用户装过更新版本的组件时，不该被包内的
// 旧副本压过。整包用户没装过组件，3 会直接命中，行为不变。
//
// 为什么随包（3）排在旧扁平（4）之前：tools/ 是构建产出的确定来源，比「手动
// 散放在同级目录」可信。手动放置的用户仍有第 1 档（配置项）可用。
package locate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"jabgrab/internal/config"
	"jabgrab/internal/downloader"
)

// Candidate 是调用方注入的一个候选路径。
//
// 存在的意义是让「按需下载的组件」参与定位，而不让 locate 反向依赖组件管理
// （那会让 locate 里出现 manifest、锁、网络 —— 它只该知道「路径能不能跑」）。
type Candidate struct {
	// Path 是可执行文件的绝对路径。
	Path string
	// Source 是给人看的来源文案，会原样显示在设置页与 --selftest 里。
	Source string
}

// Resolver 把「组件名 → 已安装组件的候选路径」注入定位链。
//
// 由 main 包实现（它才知道组件管理器的状态），locate 只调用。返回 nil 表示没有
// 装过该组件，此时定位会继续走随包/旧布局/PATH。
type Resolver func(name string) []Candidate

// 组件名。同时是清单里的键、落盘目录前缀与 Resolver 的入参，改动会让人已装的
// 组件变成「未安装」。
const (
	NameScraper = "scraper"
	NameDlr     = "dlre"
	NameFFmpeg  = "ffmpeg"
)

// From 调用 resolver，nil 安全。
func From(r Resolver, name string) []Candidate {
	if r == nil {
		return nil
	}
	return r(name)
}

// 来源文案。它们是 UI 文案而不是内部标识：五档里有两档可能指向同名的不同文件，
// 「装了组件却不生效」这类问题第一眼看的就是这一列。
const (
	SourceConfig    = "配置指定"
	SourceComponent = "组件"
	SourceBundled   = "随包"
	SourceSibling   = "程序同级目录"
	SourceDev       = "开发目录"
	SourcePath      = "系统 PATH"
)

// 随包布局的目录名。与 tools/package.sh、tools/package.ps1、CI 的断言共同
// 构成契约，改一处就要改三处。
const (
	bundledDirScraper = "tools/scraper"
	bundledDirDlr     = "tools/dlre"
	bundledDirFFmpeg  = "tools/ffmpeg"
)

// ExeDir 返回当前可执行文件所在目录。
func ExeDir() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Dir(p), nil
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func executableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode()&0o111 != 0
}

// searchRoots 返回按优先级排列的搜索根目录。
func searchRoots() []string {
	var roots []string

	if dir, err := ExeDir(); err == nil {
		roots = append(roots, dir, filepath.Join(dir, "bin"))

		// macOS: <Dist>/X.app/Contents/MacOS → 追加 <Dist> 与 <Dist>/bin
		if filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
			dist := filepath.Dir(filepath.Dir(filepath.Dir(dir)))
			roots = append(roots, dist, filepath.Join(dist, "bin"))
		}
	}

	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd, filepath.Join(wd, ".."))
	}
	return roots
}

// findExecutable 在各搜索根下按相对路径找可执行文件。
func findExecutable(rel string) (string, bool) {
	for _, root := range searchRoots() {
		cand := filepath.Join(root, rel)
		if executableFile(cand) {
			return filepath.Clean(cand), true
		}
	}
	return "", false
}

// fromCandidates 返回第一个可执行的注入候选。
func fromCandidates(cands []Candidate) (string, string, bool) {
	for _, c := range cands {
		if c.Path == "" {
			continue
		}
		if executableFile(c.Path) {
			return filepath.Clean(c.Path), c.Source, true
		}
	}
	return "", "", false
}

// Dlr 定位 N_m3u8DL-RE，返回路径与来源说明（用于 UI 展示）。
func Dlr(cfg config.Config, extra ...Candidate) (string, string, error) {
	if p := strings.TrimSpace(cfg.DlrPath); p != "" {
		if executableFile(p) {
			return p, SourceConfig, nil
		}
		return "", "", fmt.Errorf("配置中的路径不可执行: %s", p)
	}

	name := exeName("N_m3u8DL-RE")
	if p, src, ok := fromCandidates(extra); ok {
		return p, src, nil
	}
	if p, ok := findExecutable(filepath.Join(bundledDirDlr, name)); ok {
		return p, SourceBundled, nil
	}
	// 旧扁平布局（v0.3.0 及更早的 zip）：三件套与主程序同级
	if p, ok := findExecutable(name); ok {
		return p, SourceSibling, nil
	}
	// 开发布局：仓库内 third_party/dlre/<os>-<arch>/。
	// go run 的工作目录是 go/，所以要退一级才到仓库根；从仓库根跑命令时直接命中。
	devRel := filepath.Join("third_party", "dlre", runtime.GOOS+"-"+runtime.GOARCH, name)
	for _, candidate := range []string{devRel, filepath.Join("..", devRel)} {
		if p, ok := findExecutable(candidate); ok {
			return p, SourceDev, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, SourcePath, nil
	}
	return "", "", errors.New("未找到 N_m3u8DL-RE，请在设置中指定可执行文件路径，或下载组件")
}

// Scrape 定位爬取入口，返回可直接 exec 的参数列表与工作目录。
//
// 优先级：
//  1. 配置里的 PythonCmd（可带参数，如 "uv run jab-scrape"）
//  2. 注入候选（按需下载的组件）
//  3. 随包 tools/scraper/jab-scrape
//  4. 旧扁平布局：同级 / 同级 bin/ / 同级 python/
//  5. 开发模式：向上找到含 python/pyproject.toml 的目录，用 uv run --project
func Scrape(cfg config.Config, extra ...Candidate) (argv []string, dir string, err error) {
	argv, dir, _, err = ScrapeSource(cfg, extra...)
	return argv, dir, err
}

// ScrapeSource 同 Scrape，另返回命中的来源。
//
// 来源是「装了组件却不生效」这类问题的第一线索（六档里有两档可能指向同名的
// 不同文件），所以设置页与 --selftest 都要显示它。
func ScrapeSource(cfg config.Config, extra ...Candidate) (argv []string, dir string, source string, err error) {
	if raw := strings.TrimSpace(cfg.PythonCmd); raw != "" {
		argv = splitCommand(raw)
		if len(argv) == 0 {
			return nil, "", SourceConfig, errors.New("爬取命令为空")
		}
		return argv, workDirFor(argv[0]), SourceConfig, nil
	}

	bin := exeName("jab-scrape")
	if p, src, ok := fromCandidates(extra); ok {
		return []string{p}, filepath.Dir(p), src, nil
	}
	for _, candidate := range []struct {
		rel    string
		source string
	}{
		{filepath.Join(bundledDirScraper, bin), SourceBundled},
		{bin, SourceSibling},
		{filepath.Join("bin", bin), SourceSibling},
		{filepath.Join("python", bin), SourceSibling},
	} {
		if p, ok := findExecutable(candidate.rel); ok {
			return []string{p}, filepath.Dir(p), candidate.source, nil
		}
	}

	if proj, ok := findPythonProject(); ok {
		if _, e := exec.LookPath("uv"); e == nil {
			return []string{"uv", "run", "--project", proj, "jab-scrape"}, proj, SourceDev, nil
		}
		return nil, "", "", errors.New("开发模式需要 uv 才能运行 jab-scrape")
	}

	return nil, "", "", errors.New("未找到爬取程序（jab-scrape），请在设置中指定命令，或下载组件")
}

// FFmpeg 定位 ffmpeg，返回路径与来源。
//
// 它是 N_m3u8DL-RE 的**启动期硬依赖**（缺了它连下载都不开始，不是只影响混流），
// 所以调用方必须在开始任务前就拿到结果，而不是等下载时报错。
//
// 顺序：配置 → 注入候选（组件）→ 随包 → PATH 与常见 bin 目录。
// 最后一步交给 downloader.FindFFmpeg：GUI 从 Finder 启动时 PATH 往往只有
// /usr/bin:/bin，找不到 Homebrew 装的 ffmpeg，那份「常见目录」清单只维护在
// downloader 里，不在这里复刻第二份。
func FFmpeg(cfg config.Config, near []string, extra ...Candidate) (string, string) {
	if p := strings.TrimSpace(cfg.FFmpegPath); p != "" {
		if executableFile(p) {
			return p, SourceConfig
		}
		return "", "" // 配置写了但不可用：Check() 会报出来，这里不回退
	}
	if p, src, ok := fromCandidates(extra); ok {
		return p, src
	}
	if p, ok := findExecutable(filepath.Join(bundledDirFFmpeg, exeName("ffmpeg"))); ok {
		return p, SourceBundled
	}
	if p := downloader.FindFFmpeg(near...); p != "" {
		return p, SourcePath
	}
	return "", ""
}

// RequireFFmpeg 是「开始任务前」的 ffmpeg 闸门：定位失败时返回可直接展示的错误。
// 配置路径写错时明确报错（与 Dlr 的口径一致），而不是静默回退让用户猜。
func RequireFFmpeg(cfg config.Config, near []string, extra ...Candidate) (string, string, error) {
	if p := strings.TrimSpace(cfg.FFmpegPath); p != "" && !executableFile(p) {
		return "", "", fmt.Errorf("配置中的 ffmpeg 路径不可执行: %s", p)
	}
	p, src := FFmpeg(cfg, near, extra...)
	if p == "" {
		return "", "", errors.New("未找到 ffmpeg：N_m3u8DL-RE 启动时必须校验 ffmpeg，" +
			"请先安装（macOS 可 brew install ffmpeg；Windows 把 ffmpeg.exe 所在目录加入 PATH，" +
			"或把 ffmpeg 放到下载器同目录）")
	}
	return p, src, nil
}

// workDirFor 若 bin 带路径分隔符且存在，返回其所在目录，
// 便于程序找到同目录下的依赖资源。
func workDirFor(bin string) string {
	if !strings.ContainsRune(bin, filepath.Separator) && !strings.Contains(bin, "/") {
		return ""
	}
	if st, err := os.Stat(bin); err == nil && !st.IsDir() {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return ""
		}
		return filepath.Dir(abs)
	}
	return ""
}

// findPythonProject 从工作目录与可执行文件目录逐级向上找 python/pyproject.toml。
func findPythonProject() (string, bool) {
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if dir, err := ExeDir(); err == nil {
		starts = append(starts, dir)
	}

	for _, start := range starts {
		dir := start
		for i := 0; i < 6; i++ {
			cand := filepath.Join(dir, "python")
			if st, err := os.Stat(filepath.Join(cand, "pyproject.toml")); err == nil && !st.IsDir() {
				return cand, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", false
}

// splitCommand 按空白切分命令，支持单双引号包裹含空格的路径。
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inToken := false

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inToken = true
		case r == ' ' || r == '\t' || r == '\n':
			if inToken {
				out = append(out, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if inToken {
		out = append(out, cur.String())
	}
	return out
}
