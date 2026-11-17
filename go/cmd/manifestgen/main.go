// Command manifestgen 生成 Release 附带的组件清单 manifest.json，以及各平台的
// 组件自述文件。
//
// 用法（release job，资产下载齐之后执行）：
//
//	cd go && go run ./cmd/manifestgen \
//	  -dir ../release-assets \
//	  -base-url "https://github.com/o/r/releases/download/v1.2.3" \
//	  -require "windows/amd64,darwin/arm64,darwin/amd64" \
//	  -out ../release-assets/manifest.json
//
// 一次只处理一个组件，多组件发布靠 -merge 累加：
//
//	      -name scraper -binary jab-scrape  -out manifest.json
//	-merge -name dlre    -binary N_m3u8DL-RE -out manifest.json
//
// 构建阶段生成「产物自述」也用它（-meta），这样 bash 与 pwsh 都不必自己拼 JSON：
//
//	go run ./cmd/manifestgen -meta -exe ../tools/scraper/jab-scrape \
//	  -name scraper -protocol 1 -out ../out/scraper_meta_darwin_arm64.json
//
// 为什么是 Go 程序而不是 CI 里的 shell + jq
// ------------------------------------------
// manifest 的字段名必须与 App 侧解析端（internal/component 的 Manifest）逐字一致。
// 用 shell 拼 JSON，字段名就成了第二份事实，改一边忘另一边时的症状只在用户机器上
// 出现（「组件清单版本不支持」「下载地址为空」），而 CI 永远是一片绿。
//
// 这里直接复用同一个 struct 生成，并且生成后立刻用 component.ParseManifest 与
// Spec 走一遍 App 的解析与校验路径：清单 App 读不懂，构建当场失败。
//
// 输入约定
// --------
//
//	<name>_<version>_<os>_<arch>.zip    组件包
//	<name>_meta_<os>_<arch>.json        该平台产物自述的版本与协议
//
// meta 由构建阶段写入（-meta 模式）：打包完当场跑一次组件的版本命令，把输出
// 落成 JSON。于是 manifest 里的版本与协议来自**产物本身自述**，而不是 CI 手里的
// 构建变量 —— 两者一旦分离，用户看到的会是「装好了却一直提示可更新」，或版本号
// 与实际能力不符。
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"jabgrab/internal/component"
)

func main() {
	var (
		dir     = flag.String("dir", ".", "资产所在目录")
		baseURL = flag.String("base-url", "", "资产下载地址前缀（必填）")
		name    = flag.String("name", "scraper", "组件名")
		binary  = flag.String("binary", "scraper", "包内可执行文件名（不含 .exe）")
		require = flag.String("require", "", "必须齐全的平台，逗号分隔的 goos/goarch")
		only    = flag.String("only", "", "只输出这些平台，逗号分隔的 goos/goarch（用于生成 App 内置的单平台清单）")
		out     = flag.String("out", "", "输出路径，默认 <dir>/manifest.json")
		merge   = flag.Bool("merge", false, "把本组件合并进已存在的 -out 文件（多组件发布）")
		meta    = flag.Bool("meta", false, "执行 -exe 并从其版本输出生成自述 JSON，不生成清单")
		exePath = flag.String("exe", "", "-meta 模式下要执行的可执行文件")
		verArgs = flag.String("version-args", "--version", "-meta 模式下取版本的参数（按空格切分）")
		proto   = flag.Int("protocol", 0, "-meta 模式下写进自述的协议版本")
		pack    = flag.Bool("pack", false, "把 -src 目录打成组件包并同时生成自述")
		src     = flag.String("src", "", "-pack 模式下要打包的目录（内含可执行文件）")
		slug    = flag.String("slug", "", "-pack 模式下的平台名（darwin/windows），用于文件名")
		arch    = flag.String("arch", runtime.GOARCH, "-pack 模式下的架构名")
		outDir  = flag.String("out-dir", ".", "-pack 模式的输出目录")
	)
	flag.Parse()

	opts := options{
		dir:      *dir,
		baseURL:  *baseURL,
		name:     *name,
		binary:   *binary,
		required: splitList(*require),
		only:     splitList(*only),
		merge:    *merge,
	}
	if *out == "" {
		opts.out = filepath.Join(opts.dir, "manifest.json")
	} else {
		opts.out = *out
	}

	if *pack {
		if *src == "" || *slug == "" {
			fail(errors.New("-pack 需要 -src（打包目录）与 -slug（平台名）"))
		}
		if err := packComponent(packOptions{
			src:         *src,
			outDir:      *outDir,
			name:        *name,
			binary:      *binary,
			protocol:    *proto,
			slug:        *slug,
			arch:        *arch,
			versionArgs: splitArgs(*verArgs),
		}); err != nil {
			fail(err)
		}
		return
	}

	if *meta {
		if *exePath == "" {
			fail(errors.New("-meta 需要 -exe（要执行的可执行文件）"))
		}
		if err := writeMeta(*exePath, opts.out, *name, *proto, splitArgs(*verArgs)); err != nil {
			fail(err)
		}
		return
	}

	if err := run(opts); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "生成组件清单失败：%v\n", err)
	os.Exit(1)
}

func run(opts options) error {
	data, err := build(opts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(opts.out, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", opts.out, err)
	}
	fmt.Printf("已生成 %s（%d 字节）\n", opts.out, len(data))
	fmt.Println(string(data))
	return nil
}

type options struct {
	dir      string
	baseURL  string
	name     string
	binary   string
	required []string
	// only 非空时只保留这些平台。
	//
	// 用途：把「本平台的包」烘进 App 作为断网兜底。App 只需要自己平台那一份，
	// 全平台的清单塞进去是给别人用的数据，白占体积也白暴露别的平台校验码。
	only []string
	// merge 为真时把本组件并入已存在的 out 文件。
	//
	// 为什么不是「一次调用生成多个组件」：每个组件的包名、可执行文件名、版本
	// 来源都不同（scraper 来自 PyInstaller 产物，dlre 来自上游二进制），把它们
	// 塞进一次调用只会让参数变成逗号分隔的多组值，而那种参数在 shell 里错误率
	// 极高。分成多次调用、每次只描述一个组件，merge 负责拼装。
	merge bool
	out   string
}

// slugToGOOS 把发布文件名里的平台词映射到 GOOS。
//
// 只有这一处做翻译，且方向是「文件名 → GOOS」。反向查表（GOOS → 文件名）在
// 生成清单时用不上，多写一份只会多一处可能漏项的地方。
//
// darwin 与 macos 都收：grabber 的 App 包用 darwin（jabgrab_0.4.0_darwin_arm64.zip），
// 上游 N_m3u8DL-RE 的资产名用 osx（osx-arm64）。组件包统一用 darwin，而
// macos 留作容忍写法 —— 它只影响解析，不影响产物命名。
var slugToGOOS = map[string]string{
	"darwin":  "darwin",
	"macos":   "darwin",
	"windows": "windows",
	"linux":   "linux",
}

// safeAssetName 限定文件名可用字符。
//
// 文件名会原样拼进下载地址，而地址是 App 唯一能用的定位方式：一个空格或 `#`
// 就能让 URL 指向别处或截断，症状是「下载 404」，排查时很难想到是版本号里的字符。
var safeAssetName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// build 生成 manifest 的字节内容。
func build(opts options) ([]byte, error) {
	if opts.baseURL == "" {
		return nil, errors.New("缺少 -base-url（资产下载地址前缀）")
	}
	// 严格校验会拼进路径的字段：Name/Binary 来自命令行，却会决定扫描哪些文件。
	if err := safeSegment("组件名", opts.name); err != nil {
		return nil, err
	}
	if err := safeSegment("可执行文件名", opts.binary); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(opts.dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录 %s: %w", opts.dir, err)
	}

	metaPrefix := opts.name + "_meta_"
	var (
		assets   []resolved
		versions = map[string][]string{} // version → 出现在哪些平台
	)
	for _, entry := range entries {
		fileName := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(fileName, metaPrefix) || !strings.HasSuffix(fileName, ".json") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimPrefix(fileName, metaPrefix), ".json")
		slug, arch, err := splitSlugArch(stem)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fileName, err)
		}
		goos, ok := slugToGOOS[slug]
		if !ok {
			return nil, fmt.Errorf("%s: 未知平台 %q（已知：%s）", fileName, slug, knownSlugs())
		}

		version, protocol, revision, err := readMeta(filepath.Join(opts.dir, fileName), opts.name)
		if err != nil {
			return nil, err
		}
		versions[version] = append(versions[version], platformKey(goos, arch))

		// 包名按 meta 里的版本拼，而不是按目录里有哪些 zip 去反推。
		// 这样「文件名写的版本」与「产物自述的版本」不一致时，这里直接找不到文件 ——
		// 若反过来按文件名解析版本，那种不一致会被安静地写进清单。
		zipName := fmt.Sprintf("%s_%s_%s_%s.zip", opts.name, version, slug, arch)
		if !safeAssetName.MatchString(zipName) {
			return nil, fmt.Errorf("%s: 包名含不安全字符", zipName)
		}
		zipPath := filepath.Join(opts.dir, zipName)
		if _, err := os.Stat(zipPath); err != nil {
			// 这里最常见的成因不是「忘了上传」，而是构建侧用 tag 版本命名了包、
			// 而产物自述的是另一个版本。把两者同时摆出来，比一句 no such file
			// 有用得多 —— 后者会让人去查上传步骤，而问题在打包命名。
			return nil, fmt.Errorf("找不到组件包 %s（按产物自述的版本 %s 拼名）；目录里同平台的包：%s",
				zipName, version, samePlatformZips(entries, opts.name, slug, arch))
		}
		if err := inspectArchive(zipPath, opts.binary, goos); err != nil {
			return nil, err
		}
		sum, size, err := fileSHA256(zipPath)
		if err != nil {
			return nil, err
		}

		assets = append(assets, resolved{
			goos:     goos,
			goarch:   arch,
			version:  version,
			protocol: protocol,
			revision: revision,
			fileName: zipName,
			asset: component.Asset{
				URL:    strings.TrimSuffix(opts.baseURL, "/") + "/" + zipName,
				Size:   size,
				SHA256: sum,
			},
		})
	}

	if len(opts.only) > 0 {
		allowed := make(map[string]bool, len(opts.only))
		for _, key := range opts.only {
			allowed[key] = true
		}
		kept := assets[:0]
		for _, a := range assets {
			if allowed[platformKey(a.goos, a.goarch)] {
				kept = append(kept, a)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("目录 %s 里没有这些平台的产物：%s（现有：%s）",
				opts.dir, strings.Join(opts.only, ", "), describeVersions(versions))
		}
		// versions 是按全部平台统计的，过滤后要重算，否则「两个平台版本不同」
		// 会因为一个被过滤掉的平台而误报。
		assets = kept
		versions = map[string][]string{}
		for _, a := range assets {
			versions[a.version] = append(versions[a.version], platformKey(a.goos, a.goarch))
		}
	}

	if len(assets) == 0 {
		return nil, fmt.Errorf("目录 %s 里没有任何 %s*.json，组件包无法生成清单", opts.dir, metaPrefix)
	}
	if len(versions) > 1 {
		// 各平台版本不同，说明它们的产物来自不同提交。ManifestComponent 只有
		// 一个 version 字段，硬塞进去必然有一个平台被标成另一版本 —— 用户那边
		// 表现为「刚下完又提示可更新」，永远修不好。
		return nil, fmt.Errorf("各平台产物版本不一致：%s", describeVersions(versions))
	}
	// 协议也只有一个字段，但它的不一致比版本更隐蔽：清单写 v1、包实际是 v2，
	// 装完之后凭据记的是 v1，兼容性检查一路放行，直到真正调用组件时才炸。
	if err := checkProtocolAgreement(assets); err != nil {
		return nil, err
	}
	// dlre 的版本自述形如 0.6.0+<commit>：版本号取前半段进文件名，commit 单独
	// 记成 revision。三个平台的 revision 不同，说明它们来自不同的上游构建 ——
	// 版本字段看起来一样，实际是三个不同的二进制。
	if err := checkRevisionAgreement(assets); err != nil {
		return nil, err
	}

	sort.Slice(assets, func(i, j int) bool {
		return platformKey(assets[i].goos, assets[i].goarch) < platformKey(assets[j].goos, assets[j].goarch)
	})

	item := component.ManifestComponent{
		Version:  assets[0].version,
		Protocol: assets[0].protocol,
		Revision: assets[0].revision,
		Binary:   opts.binary,
		Assets:   map[string]component.Asset{},
	}
	for _, a := range assets {
		item.Assets[platformKey(a.goos, a.goarch)] = a.asset
	}

	components := map[string]component.ManifestComponent{opts.name: item}
	if opts.merge {
		existing, err := loadExisting(opts.out)
		if err != nil {
			return nil, err
		}
		for name, old := range existing {
			if name == opts.name {
				continue // 同名组件以本次为准：重跑一次生成器不该被旧结果挡住
			}
			components[name] = old
		}
	}

	data, err := json.MarshalIndent(component.Manifest{
		Schema:     1,
		Components: components,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化清单: %w", err)
	}
	data = append(data, '\n')

	if err := verify(opts, data, components); err != nil {
		return nil, err
	}
	return data, nil
}

// loadExisting 读取已存在的清单，用于 -merge。
//
// 文件不存在时返回空表：允许「第一次调用不写 -merge、后续调用写」，也允许
// 全部调用都写 -merge（CI 里两种顺序都能跑通，少一个必须记清的先决条件）。
func loadExisting(path string) (map[string]component.ManifestComponent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]component.ManifestComponent{}, nil
		}
		return nil, fmt.Errorf("读取已存在的清单 %s: %w", path, err)
	}
	// 这里刻意不用 component.ParseManifest：它把「components 为空」当成错误，
	// 而仓库里提交的兜底清单正是空壳（{"schema":1,"components":{}}），第一次
	// 生成内置清单时要能在这个空壳上累加。
	var manifest component.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("已存在的清单 %s 无法解析: %w", path, err)
	}
	if manifest.Schema != 1 {
		// schema 不对时不「当它不存在」重建：那会静默丢掉先前写入的组件，
		// 而 Release 上少一个组件只有用户点下载时才发现。
		return nil, fmt.Errorf("已存在的清单 %s 的 schema 是 %d，本程序只认 1", path, manifest.Schema)
	}
	if manifest.Components == nil {
		manifest.Components = map[string]component.ManifestComponent{}
	}
	return manifest.Components, nil
}

// verify 用 App 侧的解析路径把刚生成的清单读一遍。
//
// 这一步挡的是「生成器自己觉得没问题」：字段名拼错、schema 写成别的值、URL 协议
// 不是 http(s) —— 这些在生成端看起来都正常，只有 App 解析时才暴露，而那时清单
// 已经挂在 Release 上了。
func verify(opts options, data []byte, components map[string]component.ManifestComponent) error {
	manifest, err := component.ParseManifest(data)
	if err != nil {
		return fmt.Errorf("生成的清单无法被 App 解析: %w", err)
	}
	// 合并进来、但本轮没生成的组件也要在这里过一遍：它们的字段来自上一次调用，
	// 而那次调用的校验结果不能代替落盘之后的这一次（文件可能被手改过）。
	for name, item := range components {
		if _, ok := manifest.Components[name]; !ok {
			return fmt.Errorf("清单里缺少组件 %s", name)
		}
		if len(item.Assets) == 0 {
			return fmt.Errorf("组件 %s 没有任何平台条目", name)
		}
		for key := range item.Assets {
			goos, goarch, ok := strings.Cut(key, "/")
			if !ok {
				return fmt.Errorf("组件 %s 的平台键格式错误：%q", name, key)
			}
			// Spec 内部会调用 Validate：URL 协议、校验码格式、大小非负都在这里过一遍。
			if _, err := manifest.Spec(name, goos, goarch); err != nil {
				return fmt.Errorf("清单条目 %s/%s 无效: %w", name, key, err)
			}
		}
	}

	present := map[string]bool{}
	for key := range manifest.Components[opts.name].Assets {
		present[key] = true
	}

	// required 与 only 同时给时，只校验过滤后仍在清单里的平台：
	// 「烘内置清单」的场景下 required 是全平台的（构建侧写死），
	// 而实际产物只有本平台一份。
	required := opts.required
	if len(opts.only) > 0 {
		required = opts.only
	}
	if len(required) == 0 {
		fmt.Fprintf(os.Stderr, "警告：未指定 -require，未校验平台齐全性（现有：%s）\n", strings.Join(sortedKeys(present), ", "))
	}
	var missing []string
	for _, key := range required {
		if !present[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("清单缺少必需平台的构建：%s（现有：%s）", strings.Join(missing, ", "), strings.Join(sortedKeys(present), ", "))
	}
	return nil
}

type resolved struct {
	goos     string
	goarch   string
	version  string
	protocol int
	// revision 是版本自述里 `+` 之后的部分（上游 commit hash），没有则为空。
	revision string
	fileName string
	asset    component.Asset
}

// readMeta 读取构建阶段落盘的产物自述。
//
// 字段名同时认两种写法：`version` 与组件名同名键（如 jable-scrape 的自述里
// 可能是 `"scraper": "0.1.0"`），协议认 `protocol` 与 `v`。让生成器认两种，
// CI 就只是把命令输出重定向到文件，不必在 bash 与 pwsh 里各写一份 JSON 解析 ——
// 那才是两处迟早对不上的地方。
//
// 版本里的 `+<commit>` 在这里切开：`+` 不能进文件名（会拼进下载地址），而版本
// 自述里带 hash 的组件（N_m3u8DL-RE 就是）必须把两者分开存。
func readMeta(path, name string) (version string, protocol int, revision string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, "", fmt.Errorf("读取 %s: %w", filepath.Base(path), err)
	}
	// PowerShell 的 Set-Content 在 5.1 上会带 BOM，Go 的 json 不接受。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", 0, "", fmt.Errorf("解析 %s: %w", filepath.Base(path), err)
	}

	version = firstString(raw, "version", name)
	if version == "" {
		return "", 0, "", fmt.Errorf("%s 里没有版本号（找过 %q 与 %q）", filepath.Base(path), "version", name)
	}
	protocol, ok := firstNumber(raw, "protocol", "v")
	if !ok {
		return "", 0, "", fmt.Errorf("%s 里没有协议版本（找过 %q 与 %q）", filepath.Base(path), "protocol", "v")
	}
	revision = firstString(raw, "revision")
	if base, rev, found := strings.Cut(version, "+"); found {
		version, revision = base, rev
	}
	if !safeAssetName.MatchString(version) {
		return "", 0, "", fmt.Errorf("%s 的版本号 %q 含不能进文件名的字符（+ 之后的 commit 请写进 revision）",
			filepath.Base(path), version)
	}
	return version, protocol, revision, nil
}

// inspectArchive 确认包里真有可执行文件，且解压后正好落在组件目录顶层。
//
// 用 component.TopLevelDir 而不是自己数层级：解压侧的剥壳规则只有一份，
// 打包侧另写一份的话，两份迟早分叉 —— 分叉的症状是「Release 发布成功、
// 用户装完报缺少可执行文件」，现场完全没有线索指向打包环节。
func inspectArchive(path, binary, goos string) error {
	if !safeAssetName.MatchString(filepath.Base(path)) {
		return fmt.Errorf("%s: 包名含不安全字符", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开组件包 %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("读取组件包 %s: %w", filepath.Base(path), err)
	}
	zr, err := zip.NewReader(file, info.Size())
	if err != nil {
		return fmt.Errorf("%s 不是有效的 zip: %w", filepath.Base(path), err)
	}

	want := component.TopLevelDir(zr.File) + binary + exeSuffix(goos)
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		if f.Name == want && f.Mode().IsRegular() {
			// 可执行位也要核：包内条目是 0644 时解出来就是 0644，装完之后
			// State 报 Corrupt，用户看到的是「装好了却跑不起来」。
			// Windows 的可执行性由扩展名决定，没有权限位这一说。
			if goos != "windows" && f.Mode().Perm()&0o111 == 0 {
				return fmt.Errorf("%s 里的 %s 没有可执行权限（mode %v）；zip 打包时要保留权限位（macOS 用 zip -y）",
					filepath.Base(path), want, f.Mode())
			}
			return nil
		}
		if len(names) < 5 {
			names = append(names, f.Name)
		}
	}
	// 报错要带上实际看到的名字：只说「缺少可执行文件」的话，无从判断是没打进去、
	// 名字不对，还是多包了一层目录。
	return fmt.Errorf("%s 里没有 %s（解压后应落在顶层）；包内前几条：%s",
		filepath.Base(path), want, strings.Join(names, ", "))
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("打开 %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, fmt.Errorf("读取 %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// splitSlugArch 从 "macos_arm64" 这种后缀里切出平台与架构。
func splitSlugArch(stem string) (slug, arch string, err error) {
	idx := strings.LastIndexByte(stem, '_')
	if idx <= 0 || idx == len(stem)-1 {
		return "", "", fmt.Errorf("文件名应形如 <os>_<arch>，实际 %q", stem)
	}
	slug, arch = stem[:idx], stem[idx+1:]
	// slug 本身不含分隔符，所以再切一次必须失败；否则说明格式不是预期的三段式。
	if strings.ContainsRune(slug, '_') {
		return "", "", fmt.Errorf("文件名应形如 <os>_<arch>，实际 %q", stem)
	}
	return slug, arch, nil
}

// checkProtocolAgreement 要求每个平台的产物自述协议一致。
//
// 比版本不一致更隐蔽：清单里写一个协议值，包实际是另一个，装完之后凭据记的
// 也是清单里的值 —— 兼容性检查一路放行，直到真正调用组件时才炸，而那时用户
// 看到的是一句与协议毫无关系的错误。
func checkProtocolAgreement(assets []resolved) error {
	first := assets[0]
	var drift []string
	for _, a := range assets[1:] {
		if a.protocol != first.protocol {
			drift = append(drift, fmt.Sprintf("%s=v%d", platformKey(a.goos, a.goarch), a.protocol))
		}
	}
	if len(drift) == 0 {
		return nil
	}
	return fmt.Errorf("各平台产物协议版本不一致：%s=v%d，%s",
		platformKey(first.goos, first.goarch), first.protocol, strings.Join(drift, "，"))
}

// checkRevisionAgreement 要求每个平台的 revision（上游 commit）一致。
//
// 这是 grabber 特有的一条：N_m3u8DL-RE 的版本自述是 `0.6.0+<commit>`，切开之后
// version 相同、revision 不同是完全可能的（上游对同一版本号重发过构建）。
// 两者都进清单，但 ManifestComponent 只有一个 version 字段 —— revision 不一致
// 时，三个平台的用户拿到的其实是三个不同的二进制，而界面显示同一个版本号。
func checkRevisionAgreement(assets []resolved) error {
	first := assets[0]
	var drift []string
	for _, a := range assets[1:] {
		if a.revision != first.revision {
			drift = append(drift, fmt.Sprintf("%s=%s", platformKey(a.goos, a.goarch), shortOrNone(a.revision)))
		}
	}
	if len(drift) == 0 {
		return nil
	}
	return fmt.Errorf("各平台产物的 revision 不一致：%s=%s，%s",
		platformKey(first.goos, first.goarch), shortOrNone(first.revision), strings.Join(drift, "，"))
}

func shortOrNone(value string) string {
	if value == "" {
		return "（无）"
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// packOptions 描述一次「打包组件」的输入。
type packOptions struct {
	src         string
	outDir      string
	name        string
	binary      string
	protocol    int
	slug        string
	arch        string
	versionArgs []string
}

// packComponent 把目录打成组件包，并按产物自述的版本命名，同时写自述文件。
//
// 为什么打包也放进这个程序
// ------------------------
// 包名里的版本必须与自述的版本逐字一致（生成清单时按它拼文件名去查找），而版本
// 只能从产物里读。让 CI 的 shell 先跑一次 `--version`、切出 `+` 之前的部分、
// 拼出文件名、再分别用 `zip -y`（macOS）与 `Compress-Archive`（Windows）打包，
// 等于把「包名 = 自述版本」这条不变量拆成两份平台相关的实现 —— 而它们分叉时
// 的表现是构建成功、用户装完报「缺少可执行文件」。
//
// 顶层目录 `<name>/` 也在这里加：解压侧靠 component.TopLevelDir 剥掉它。
func packComponent(opts packOptions) error {
	if err := safeSegment("组件名", opts.name); err != nil {
		return err
	}
	if err := safeSegment("可执行文件名", opts.binary); err != nil {
		return err
	}
	goos, ok := slugToGOOS[opts.slug]
	if !ok {
		return fmt.Errorf("未知平台 %q（已知：%s）", opts.slug, knownSlugs())
	}

	exePath := filepath.Join(opts.src, opts.binary+exeSuffix(goos))
	if _, err := os.Stat(exePath); err != nil {
		return fmt.Errorf("打包目录里没有 %s: %w", filepath.Base(exePath), err)
	}

	version, revision, err := componentVersion(exePath, opts.versionArgs)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录 %s: %w", opts.outDir, err)
	}

	metaName := fmt.Sprintf("%s_meta_%s_%s.json", opts.name, opts.slug, opts.arch)
	metaPath := filepath.Join(opts.outDir, metaName)
	payload := map[string]any{"version": version, "protocol": opts.protocol, "os": goos, "arch": opts.arch}
	if revision != "" {
		payload["revision"] = revision
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化自述: %w", err)
	}
	if err := os.WriteFile(metaPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", metaPath, err)
	}

	zipName := fmt.Sprintf("%s_%s_%s_%s.zip", opts.name, version, opts.slug, opts.arch)
	if !safeAssetName.MatchString(zipName) {
		return fmt.Errorf("包名 %q 含不安全字符", zipName)
	}
	zipPath := filepath.Join(opts.outDir, zipName)
	if err := writeComponentZip(opts.src, zipPath, opts.name); err != nil {
		return err
	}
	// 打完立刻按解压侧的规则校验一遍：顶层目录、可执行位、名字对不对。
	if err := inspectArchive(zipPath, opts.binary, goos); err != nil {
		return err
	}
	fmt.Printf("已生成 %s 与 %s（%s %s，protocol=%d）\n", zipName, metaName, version, shortOrNone(revision), opts.protocol)
	return nil
}

// writeComponentZip 把 src 目录打成一个「顶层带组件名」的 zip。
//
// 符号链接直接报错而不同步复制：复制会把链接换成实体文件，装出来的目录与构建
// 目录不同，而这种差异只在某个动态库被重复加载时才发作。当前产物没有链接
// （PyInstaller onedir 是普通文件树），真出现了应该显式处理，而不是悄悄改形状。
func writeComponentZip(src, zipPath, name string) error {
	file, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("创建 %s: %w", zipPath, err)
	}
	zw := zip.NewWriter(file)

	walkErr := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s 是符号链接，组件包暂不支持（见 writeComponentZip 注释）", rel)
		}
		entryName := name + "/" + filepath.ToSlash(rel)
		if entry.IsDir() {
			// 显式写目录条目：解压侧的 TopLevelDir 认「以 name/ 开头」的条目，
			// 而 zip 里只有文件条目也能过 —— 但空目录会丢。多写一条更接近
			// `zip -r` 的形状。
			header := &zip.FileHeader{Name: entryName + "/"}
			header.SetMode(os.ModeDir | 0o755)
			_, err := zw.CreateHeader(header)
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = entryName
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		_, err = io.Copy(writer, src)
		return err
	})
	if walkErr != nil {
		_ = zw.Close()
		_ = file.Close()
		return fmt.Errorf("打包 %s: %w", src, walkErr)
	}
	if err := zw.Close(); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入 %s: %w", zipPath, err)
	}
	return file.Close()
}

// componentVersion 执行组件的版本命令，返回切分后的版本与 revision。
func componentVersion(exePath string, versionArgs []string) (string, string, error) {
	if len(versionArgs) == 0 {
		return "", "", errors.New("取版本的参数为空")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exePath, versionArgs...)
	outBytes, err := cmd.Output()
	if err != nil {
		// 把 stderr 一起带出来：产物在 CI 里跑不起来时，错误信息就是唯一线索。
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", "", fmt.Errorf("执行 %s %s 失败: %w（stderr: %s）",
				filepath.Base(exePath), strings.Join(versionArgs, " "), err,
				strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", fmt.Errorf("执行 %s %s 失败: %w", filepath.Base(exePath), strings.Join(versionArgs, " "), err)
	}

	line := strings.TrimSpace(strings.SplitN(string(outBytes), "\n", 2)[0])
	if line == "" {
		return "", "", fmt.Errorf("%s 的版本命令没有输出", filepath.Base(exePath))
	}
	version, revision := line, ""
	if base, rev, found := strings.Cut(line, "+"); found {
		version, revision = base, rev
	}
	if !safeAssetName.MatchString(version) {
		return "", "", fmt.Errorf("版本号 %q 含不能进文件名的字符", version)
	}
	return version, revision, nil
}

// writeMeta 执行组件可执行文件的版本命令，把结果落成自述 JSON。
//
// 为什么由这个程序来做而不是 CI 的 shell：要在 bash 与 pwsh 里各写一遍「跑命令、
// 取首行、切 +、拼 JSON、校验字符」，两份实现必然分叉。让两个平台的 CI 都调
// 同一条命令，差别就只剩路径分隔符。
func writeMeta(exePath, out, name string, protocol int, versionArgs []string) error {
	if err := safeSegment("组件名", name); err != nil {
		return err
	}
	version, revision, err := componentVersion(exePath, versionArgs)
	if err != nil {
		return err
	}

	payload := map[string]any{"version": version, "protocol": protocol, "os": runtime.GOOS, "arch": runtime.GOARCH}
	if revision != "" {
		payload["revision"] = revision
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化自述: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("创建目录 %s: %w", filepath.Dir(out), err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", out, err)
	}
	fmt.Printf("已生成 %s：%s（protocol=%d）\n", out, version, protocol)
	return nil
}

// splitArgs 按空白切分命令参数（支持 -version-args "-version"）。
func splitArgs(value string) []string {
	return strings.Fields(value)
}

// samePlatformZips 列出目录里属于同一平台的包名，用于「找不到期望版本」时的诊断。
func samePlatformZips(entries []os.DirEntry, name, slug, arch string) string {
	suffix := "_" + slug + "_" + arch + ".zip"
	var found []string
	for _, entry := range entries {
		fileName := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(fileName, name+"_") || !strings.HasSuffix(fileName, suffix) {
			continue
		}
		found = append(found, fileName)
	}
	if len(found) == 0 {
		return "无"
	}
	sort.Strings(found)
	return strings.Join(found, ", ")
}

func platformKey(goos, goarch string) string { return goos + "/" + goarch }

func exeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

func safeSegment(what, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%s为空", what)
	case strings.ContainsAny(value, `/\`), strings.ContainsRune(value, 0), value == ".", value == "..":
		return fmt.Errorf("%s非法：%q", what, value)
	}
	return nil
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := raw[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func firstNumber(raw map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if f, ok := raw[key].(float64); ok {
			return int(f), true
		}
	}
	return 0, false
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func knownSlugs() string {
	slugs := make([]string, 0, len(slugToGOOS))
	for slug := range slugToGOOS {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return strings.Join(slugs, ", ")
}

func describeVersions(versions map[string][]string) string {
	parts := make([]string, 0, len(versions))
	for version, platforms := range versions {
		sort.Strings(platforms)
		parts = append(parts, fmt.Sprintf("%s（%s）", version, strings.Join(platforms, ", ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "；")
}
