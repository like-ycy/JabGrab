package component

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
)

// Manifest 是 Release 附带的组件清单（manifest.json）。
//
// 它存在的意义是让「要下什么、下多大的、校验码是多少」可以随发布更新，
// 而不必发一版 App。App 侧另需一份内置兜底（接入时由 CI 用 ldflags 注入
// 版本与校验码），断网时至少能告诉用户需要哪个版本，而不是显示「未知」。
type Manifest struct {
	Schema     int                          `json:"schema"`
	Components map[string]ManifestComponent `json:"components"`
}

// ManifestComponent 是一个组件在 manifest 里的条目。
type ManifestComponent struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	// Revision 是版本自述里 `+` 之后的部分（上游 commit hash），没有则为空。
	//
	// 它不进安装判定，只做跨平台一致性断言与展示：N_m3u8DL-RE 的版本自述是
	// `0.6.0+<commit>`，而 version 字段装不下 hash —— 上游若对同一版本号重发
	// 过构建，三个平台的二进制其实不同，而界面会显示成同一个版本。
	Revision string `json:"revision,omitempty"`
	// Binary 是目录内可执行文件名，不含后缀；为空时取组件名。
	Binary string `json:"binary"`
	// Assets 以 Go 的 GOOS/GOARCH 为键（"windows/amd64"、"darwin/arm64"）。
	//
	// 刻意不用发布文件名里的 "macos"：那套命名是给人看的，映射到 GOOS 需要
	// 一张对照表，而对照表迟早会漏掉某一个（历史上就是 linux/arm64 漏了）。
	// GOOS 是编译器已经确定的字符串，没有翻译环节就没有漂移。
	Assets map[string]Asset `json:"assets"`
}

// ParseManifest 解析 manifest.json。
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("解析组件清单: %w", err)
	}
	if m.Schema != pointerSchema {
		return Manifest{}, fmt.Errorf("组件清单版本不支持：%d", m.Schema)
	}
	if len(m.Components) == 0 {
		return Manifest{}, errors.New("组件清单为空")
	}
	return m, nil
}

// Spec 取出指定平台上某个组件的安装描述。
//
// goos/goarch 显式传入而不是直接读 runtime：交叉编译期测试与「给另一个平台
// 准备组件」都需要指定平台，隐式读环境变量的代码无法测。
func (m Manifest) Spec(name, goos, goarch string) (Spec, error) {
	item, ok := m.Components[name]
	if !ok {
		return Spec{}, fmt.Errorf("组件清单里没有 %s", name)
	}
	asset, ok := item.Assets[platformKey(goos, goarch)]
	if !ok {
		return Spec{}, fmt.Errorf("组件 %s 没有 %s/%s 的构建", name, goos, goarch)
	}
	spec := Spec{
		Name:     name,
		Version:  item.Version,
		Protocol: item.Protocol,
		Binary:   item.Binary,
		Asset:    asset,
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("组件 %s 的清单条目无效: %w", name, err)
	}
	return spec, nil
}

// SpecForCurrent 取当前平台的安装描述。
func (m Manifest) SpecForCurrent(name string) (Spec, error) {
	return m.Spec(name, runtime.GOOS, runtime.GOARCH)
}

func platformKey(goos, goarch string) string {
	return goos + "/" + goarch
}

// URLFor 在设置了镜像前缀时改写下载地址。见 ApplyMirror。
func (a Asset) URLFor(mirror string) string {
	return ApplyMirror(mirror, a.URL)
}

// validateSHA256 要求 64 位十六进制。
//
// 校验码格式必须在安装前拒绝，而不是留给下载完再比对：写错的校验码会让每次
// 安装都在下载 20MB 之后失败，用户看到的是「网络不好」。
func validateSHA256(value string) error {
	if len(value) != 64 {
		return fmt.Errorf("组件包校验码必须是 64 位十六进制，实际 %d 位", len(value))
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return fmt.Errorf("组件包校验码含非法字符 %q", r)
		}
	}
	return nil
}
