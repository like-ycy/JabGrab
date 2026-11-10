// Package config 负责配置的读写、默认值与校验。
//
// 配置持久化为用户配置目录下的 config.json：
// macOS   ~/Library/Application Support/jabgrab/config.json
// Windows %APPDATA%\jabgrab\config.json
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	appDir   = "jabgrab"
	fileName = "config.json"
)

// Site 是一个站点的开关与匹配关键字。
// URL 中包含 Keyword（不区分大小写）即判定属于该站点。
type Site struct {
	Enabled bool   `json:"enabled"`
	Keyword string `json:"keyword"`
}

// Download 是透传给 N_m3u8DL-RE 的下载参数。
type Download struct {
	Threads    int    `json:"threads"`    // --thread-count
	RetryCount int    `json:"retryCount"` // --download-retry-count
	TimeoutSec int    `json:"timeoutSec"` // --http-request-timeout
	MaxSpeed   string `json:"maxSpeed"`   // --max-speed，空表示不限速，如 "15M"
	MuxToMP4   bool   `json:"muxToMP4"`   // 下载完混流成 mp4
}

// Config 是应用的全部可配置项。
type Config struct {
	// 路径
	SaveDir   string   `json:"saveDir"`   // 视频保存目录
	TxtPath   string   `json:"txtPath"`   // 待下载 URL 列表
	RecentTxt []string `json:"recentTxt"` // 最近用过的 txt
	DlrPath   string   `json:"dlrPath"`   // N_m3u8DL-RE 路径，空则用同级目录/PATH
	PythonCmd string   `json:"pythonCmd"` // 爬取命令，空则自动探测

	// 站点
	Sites map[string]Site `json:"sites"`

	// 下载参数
	Download Download `json:"download"`

	// 爬取超时（秒）。开浏览器比下载慢，单独设。
	ScrapeTimeoutSec int `json:"scrapeTimeoutSec"`
}

// Default 返回一份可用的默认配置。
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		SaveDir:   filepath.Join(home, "Downloads", "JabGrab"),
		RecentTxt: []string{},
		Sites: map[string]Site{
			"jable":  {Enabled: true, Keyword: "jable"},
			"missav": {Enabled: true, Keyword: "missav"},
		},
		Download: Download{
			Threads:    25,
			RetryCount: 20,
			TimeoutSec: 30,
			MaxSpeed:   "",
			MuxToMP4:   true,
		},
		ScrapeTimeoutSec: 180,
	}
}

// Dir 返回配置文件所在目录。
// 设置环境变量 JABGRAB_CONFIG_DIR 可覆盖（便携模式/测试用）。
func Dir() (string, error) {
	if custom := strings.TrimSpace(os.Getenv("JABGRAB_CONFIG_DIR")); custom != "" {
		return custom, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法定位用户配置目录: %w", err)
	}
	return filepath.Join(base, appDir), nil
}

// Path 返回配置文件完整路径。
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, fileName), nil
}

// Load 读取配置。文件不存在时返回默认配置（不落盘）。
func Load() (Config, error) {
	p, err := Path()
	if err != nil {
		return Default(), err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Default(), fmt.Errorf("读取配置失败: %w", err)
	}

	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("解析配置失败: %w", err)
	}
	cfg.normalize()
	return cfg, nil
}

// Exists 报告配置文件是否已存在。
//
// 用于区分「首次运行」和「文件读了但解析失败」：后者绝不能拿默认值覆盖，
// 否则用户手改出语法错误的配置会被直接冲掉。
func Exists() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Save 把配置写入用户配置目录，并返回补全默认值之后的配置。
//
// 返回值务必拿去更新运行态：normalize 作用在副本上，若调用方继续用传入的
// 原值，就会出现「盘上是对的、内存里是空的」这种前后不一致。
func Save(cfg Config) (Config, error) {
	cfg.normalize()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return cfg, fmt.Errorf("创建配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return cfg, fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil {
		return cfg, fmt.Errorf("写入配置失败: %w", err)
	}
	return cfg, nil
}

// normalize 给缺失/非法的字段补默认值，避免旧配置少字段时行为突变。
func (c *Config) normalize() {
	def := Default()

	if strings.TrimSpace(c.SaveDir) == "" {
		c.SaveDir = def.SaveDir
	}
	if c.Sites == nil {
		c.Sites = def.Sites
	}
	// 缺站点时补上默认站点；已有的保留用户设置。
	for name, site := range def.Sites {
		if _, ok := c.Sites[name]; !ok {
			c.Sites[name] = site
			continue
		}
		s := c.Sites[name]
		if strings.TrimSpace(s.Keyword) == "" {
			s.Keyword = site.Keyword
		}
		c.Sites[name] = s
	}
	if c.Download.Threads <= 0 {
		c.Download.Threads = def.Download.Threads
	}
	if c.Download.RetryCount <= 0 {
		c.Download.RetryCount = def.Download.RetryCount
	}
	if c.Download.TimeoutSec <= 0 {
		c.Download.TimeoutSec = def.Download.TimeoutSec
	}
	if c.ScrapeTimeoutSec <= 0 {
		c.ScrapeTimeoutSec = def.ScrapeTimeoutSec
	}
	if c.RecentTxt == nil {
		c.RecentTxt = []string{}
	}
}

// SiteNames 返回全部站点名，按字典序保证 UI 顺序稳定。
func (c Config) SiteNames() []string {
	names := make([]string, 0, len(c.Sites))
	for name := range c.Sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MatchSite 按已启用站点的关键字判定 URL 归属。
// 命中多个时取关键字最长的那个（更具体的优先）。
func (c Config) MatchSite(rawURL string) (string, bool) {
	lower := strings.ToLower(rawURL)
	best, bestLen := "", 0
	for _, name := range c.SiteNames() {
		site := c.Sites[name]
		if !site.Enabled {
			continue
		}
		kw := strings.ToLower(strings.TrimSpace(site.Keyword))
		if kw == "" || !strings.Contains(lower, kw) {
			continue
		}
		if len(kw) > bestLen {
			best, bestLen = name, len(kw)
		}
	}
	return best, best != ""
}

// PushRecentTxt 把 path 记到最近使用列表头部，最多保留 8 条。
func (c *Config) PushRecentTxt(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	out := []string{path}
	for _, p := range c.RecentTxt {
		if p != path {
			out = append(out, p)
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	c.RecentTxt = out
}

// Check 返回配置校验问题（面向用户的中文提示），空表示一切正常。
func (c Config) Check() []string {
	var issues []string

	if strings.TrimSpace(c.SaveDir) == "" {
		issues = append(issues, "未设置视频保存路径")
	} else if err := ensureWritable(c.SaveDir); err != nil {
		issues = append(issues, fmt.Sprintf("保存路径不可写: %v", err))
	}

	if strings.TrimSpace(c.TxtPath) == "" {
		issues = append(issues, "未选择待下载 txt 文件")
	} else if st, err := os.Stat(c.TxtPath); err != nil {
		issues = append(issues, fmt.Sprintf("txt 文件不存在: %s", c.TxtPath))
	} else if st.IsDir() {
		issues = append(issues, "txt 路径指向的是目录")
	}

	if strings.TrimSpace(c.DlrPath) != "" {
		if err := executableOK(c.DlrPath); err != nil {
			issues = append(issues, fmt.Sprintf("N_m3u8DL-RE 不可用: %v", err))
		}
	}

	if len(c.enabledSites()) == 0 {
		issues = append(issues, "没有启用任何站点，URL 不会被处理")
	}
	return issues
}

func (c Config) enabledSites() []string {
	var out []string
	for _, name := range c.SiteNames() {
		if c.Sites[name].Enabled {
			out = append(out, name)
		}
	}
	return out
}

func executableOK(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("是目录而非可执行文件")
	}
	// Windows 上普通文件的权限位恒为 0666，不含执行位，按 Unix 规则判断会误报。
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		return fmt.Errorf("没有执行权限")
	}
	return nil
}

func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".jabgrab-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
