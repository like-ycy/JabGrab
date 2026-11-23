package component

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Install 下载、校验并安装组件，返回可执行文件路径。
//
// 已经是期望版本且文件完整时直接返回现有路径（幂等）：调用方可以在启动时无脑
// 调一次，不必自己先查状态 —— 多一处「要不要装」的判断，就多一处与安装逻辑
// 不一致的机会。
//
// 幂等分支为什么要跑一遍 Verify：State 只检查目录、可执行文件与协议（每次
// 启动都要调，不能逐文件 stat）。但当 _internal 里的动态库被删掉或被杀软隔离
// 时，主程序还在，State 仍是 Ready。此时若直接「已安装，返回」，用户就再也
// 装不回来了 —— 刮削每次都失败，而重装按钮点了没有任何效果。Verify 只是 55 次
// stat，代价可忽略，换的是「损坏可自愈」。
//
// 落盘顺序（每一步都是为了某类真实故障，不要调整）
// ------------------------------------------------
//  1. 下载到 <root>/.staging-<rand>/<name>.zip —— 不直接写目标目录。
//  2. 校验 sha256 与字节数。不符就删 staging，目标目录从未被碰过。
//  3. 解压到 <root>/.staging-<rand>/unpack/ —— 仍在 staging 内，原因同上。
//  4. 写凭据文件（版本、协议、文件清单）。
//  5. rename(unpack → <root>/<name>-<version>)。同一父目录下，必须同盘：
//     Windows 上跨盘 rename 会失败，而把 staging 放到系统临时目录正好会跨盘。
//  6. 最后写指针。指针在 rename 之后写，中途崩溃的后果是「多一个没人引用的
//     目录」，由 CleanupStale 回收 —— 反过来（先写指针）会让 App 指向一个
//     还没就位的目录，表现为「刮削器找不到」。
//
// 半成品目录为什么要隔离：目标目录一旦以半成品形态出现，toolpath 会找到它、
// exec 会尝试运行它、失败信息是「刮削器异常退出」。用户和日志都会被引到
// 「刮削器有 bug」上，而真实原因是「文件没下完」。
func (m *Manager) Install(ctx context.Context, spec Spec, progress ProgressFunc) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}

	if err := m.lock(ctx); err != nil {
		return "", err
	}
	defer m.unlock()

	if path, ok := m.reusable(spec); ok {
		return path, nil
	}

	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return "", fmt.Errorf("创建组件目录 %s: %w", m.root, err)
	}

	// 跨进程互斥：两个 App 实例同时安装会互相删对方的 staging 目录，
	// 表现为「下载完了却报 payload.zip 不存在」。m.mu 只保护当前实例。
	release, err := m.acquireLock(ctx, installLockWait)
	if err != nil {
		return "", err
	}
	defer release()

	// 拿到锁之后再判一次：等锁期间另一个实例可能已经装好了。
	if path, ok := m.reusable(spec); ok {
		return path, nil
	}

	// 空间检查只是提前拦一道，失败不阻断安装：包体大小可能未知（manifest 里
	// 写了 0），而各平台查询可用空间的接口也不是人人可用。
	if spec.Asset.Size > 0 {
		need := uint64(spec.Asset.Size) * 2
		if free, err := freeSpace(m.root); err == nil && free < need {
			return "", fmt.Errorf("磁盘空间不足：需要约 %s，可用 %s", humanBytes(int64(need)), humanBytes(int64(free)))
		}
	}

	staging, err := os.MkdirTemp(m.root, stagingPrefix)
	if err != nil {
		return "", fmt.Errorf("创建暂存目录: %w", err)
	}
	// 成功路径下 staging 已被 rename 走，这里只剩壳；失败路径下整个删掉。
	defer func() {
		if err := os.RemoveAll(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("警告：清理暂存目录 %s 失败：%v", staging, err)
		}
	}()

	download := filepath.Join(staging, "payload.zip")
	sum, written, err := m.download(ctx, spec, download, progress)
	if err != nil {
		return "", err
	}
	if spec.Asset.Size > 0 && written != spec.Asset.Size {
		return "", fmt.Errorf("组件包大小不符：期望 %s，实际 %s", humanBytes(spec.Asset.Size), humanBytes(written))
	}
	if !strings.EqualFold(sum, spec.Asset.SHA256) {
		return "", fmt.Errorf("组件包校验失败：期望 %s，实际 %s", spec.Asset.SHA256, sum)
	}
	// 取消可能发生在最后一刻：这时校验已经过了，但用户已经不想装了。
	// 不检查的话会继续解压、替换目录、激活新版本 —— 一次「取消」变成了
	// 「取消但它还是装上了」，下次启动用的就是新版本。
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("安装已取消: %w", err)
	}

	unpacked := filepath.Join(staging, "unpack")
	files, err := extract(ctx, download, unpacked)
	if err != nil {
		return "", err
	}

	binaryBase := spec.binaryBase()
	binaryName := exeName(binaryBase)
	if !containsFile(files, binaryName) {
		return "", fmt.Errorf("组件包内缺少可执行文件 %s", binaryName)
	}
	// 只核对名字不够：包内条目权限是 0644 时解出来就是 0644，装完之后
	// State 立刻报 Corrupt，而用户看到的是「装好了却跑不起来」，重装也没用
	// （幂等分支会命中）。在这里拒绝，旧版本原样保留，安装失败的语义才成立。
	if !isExecutableFile(filepath.Join(unpacked, binaryName)) {
		return "", fmt.Errorf("组件包内 %s 没有可执行权限", binaryName)
	}

	rc := receipt{
		Name:          spec.Name,
		Version:       spec.Version,
		Protocol:      spec.Protocol,
		Binary:        binaryBase,
		ArchiveSHA256: strings.ToLower(spec.Asset.SHA256),
		InstalledAt:   time.Now().UTC(),
		Files:         files,
	}
	if err := writeReceipt(unpacked, rc); err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("安装已取消: %w", err)
	}

	target := filepath.Join(m.root, spec.Name+"-"+spec.Version)
	swap, err := m.replaceDir(unpacked, target)
	if err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		// 指针还没写：把旧目录放回去。不回滚的话，用户点了取消，磁盘上生效的
		// 却已经是新版本 —— 而 Install 的返回值说「已取消」。
		swap.rollback()
		return "", fmt.Errorf("安装已取消: %w", err)
	}

	entry := pointerEntry{Version: spec.Version, Protocol: spec.Protocol, Dir: filepath.Base(target)}
	if err := m.upsertPointer(spec.Name, entry); err != nil {
		// 指针没写成，这份新目录没人引用；旧目录放回去，安装退回原状。
		swap.rollback()
		return "", err
	}
	// 到这里才算提交成功，旧目录可以删了。
	swap.discard()

	path := filepath.Join(target, exeName(binaryBase))
	if !isRegularFile(path) {
		return "", fmt.Errorf("安装后仍找不到可执行文件 %s", path)
	}
	return path, nil
}

// reusable 判断已安装的组件能否直接复用，返回其可执行文件路径。
//
// 三个条件缺一不可：
//   - 版本与协议都对得上（State == Ready）
//   - 文件逐个核对无缺失（Verify，见 Install 的注释）
//   - 安装凭据里记录的包校验码与本次期望的一致
//
// 第三条是为「同一版本号重新发布」准备的。Release 流程用 `gh release upload
// --clobber` 覆盖资产，版本号常常不变而内容变了；只比版本的话，用户手里那份
// 旧包永远不会被替换，而他看到的是「已是最新」。这条修复本身也很便宜：凭据
// 就在磁盘上，读一次小文件。
func (m *Manager) reusable(spec Spec) (string, bool) {
	if m.State(spec) != Ready {
		return "", false
	}
	path := m.Path(spec.Name)
	if path == "" {
		return "", false
	}
	if err := m.verifyLocked(spec.Name); err != nil {
		log.Printf("组件 %s 文件不完整（%v），重新安装", spec.Name, err)
		return "", false
	}
	entry, ok := m.lookup(spec.Name)
	if !ok {
		return "", false
	}
	rc, err := readReceipt(filepath.Join(m.root, entry.Dir))
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(rc.ArchiveSHA256, spec.Asset.SHA256) {
		log.Printf("组件 %s 的发布包已更新（已装 %s，期望 %s），重新安装",
			spec.Name, shortSHA(rc.ArchiveSHA256), shortSHA(spec.Asset.SHA256))
		return "", false
	}
	return path, true
}

// shortSHA 截短校验码，只用于日志。
func shortSHA(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// replaceDir 把 src 移到位到 dst，dst 已存在时先把它挪开。
//
// 为什么不是「先 RemoveAll 再 rename」
// ------------------------------------
// 同版本重装（发布包内容变了、版本号没变）时 dst 正是指针当前指向的那份安装，
// 不是残留。RemoveAll 不是原子的：Windows 上文件被占用、Linux 上子目录不可写，
// 它删到一半就返回错误 —— 旧主程序和凭据已经没了，指针还指着这个残缺目录，
// 结果是「本来能用，更新失败后彻底不能用」。
//
// 先整体 rename 到同目录下的 .old-<纳秒>：同一父目录内 rename 是原子的，失败时
// 旧目录原封不动。
//
// 旧目录**不在这里删**，要等指针写成功。删早了的话，后面那句 ctx 检查或
// upsertPointer 一旦失败，Install 报告「安装失败」而磁盘上生效的已经是新版，
// 旧版又没了 —— 调用方没有任何办法退回原状。
func (m *Manager) replaceDir(src, dst string) (dirSwap, error) {
	var swap dirSwap
	swap.target = dst

	if _, err := os.Stat(dst); err == nil {
		swap.backup = fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
		if err := os.Rename(dst, swap.backup); err != nil {
			return dirSwap{}, fmt.Errorf("移开旧版本目录 %s 失败（可能仍有进程在使用它）：%w", dst, err)
		}
	}

	if err := os.Rename(src, dst); err != nil {
		swap.rollback()
		return dirSwap{}, fmt.Errorf("安装到 %s 失败: %w", dst, err)
	}
	return swap, nil
}

// dirSwap 是一次目录替换的结果：新目录已就位，旧目录暂存在 backup。
//
// 两个收尾动作互斥，且都只在失败可容忍的前提下尽力而为：
//   - discard：提交成功，删掉旧目录；删不掉就留给 CleanupStale（它按「是否被
//     指针引用」判断，那时旧目录已经没人引用）。
//   - rollback：提交失败，把旧目录放回去，让磁盘回到调用前那个「能用的版本」。
type dirSwap struct {
	target string
	backup string
}

func (s dirSwap) discard() {
	if s.backup == "" {
		return
	}
	if err := os.RemoveAll(s.backup); err != nil {
		log.Printf("警告：清理旧版本目录 %s 失败（下次启动会再试）：%v", s.backup, err)
	}
}

func (s dirSwap) rollback() {
	if s.backup == "" {
		return
	}
	// 新目录可能已经就位（rename 成功但后续步骤失败），先清掉再放回旧目录。
	// 清不掉的情况（Windows 上还有进程占着）保留新版本：组件仍可用，
	// 只是没有回退 —— 比「组件消失」好。
	if err := os.RemoveAll(s.target); err != nil {
		log.Printf("警告：回滚时无法清除新版本目录 %s（%v），保留新版本", s.target, err)
		return
	}
	if err := os.Rename(s.backup, s.target); err != nil {
		log.Printf("警告：回滚到旧版本失败（%v），旧目录仍在 %s，下次启动会清理", err, s.backup)
	}
}

// download 把组件包下载到 dest，返回 sha256 与字节数。
func (m *Manager) download(ctx context.Context, spec Spec, dest string, progress ProgressFunc) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.Asset.URL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("构造下载请求: %w", err)
	}
	req.Header.Set("User-Agent", "jabgrab-component/1")
	// 明确要求未压缩的原始字节：sha256 是对发布包算的，中间层再做一次
	// gzip 编码不会改变校验结果，但会让 Content-Length 与实际写入量对不上。
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("下载组件失败（%s）: %w", spec.Asset.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("下载组件失败：HTTP %d", resp.StatusCode)
	}

	total := spec.Asset.Size
	if resp.ContentLength > 0 {
		total = resp.ContentLength
	}

	file, err := os.Create(dest)
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件 %s: %w", dest, err)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	sink := &progressWriter{
		w:        io.MultiWriter(file, hasher),
		total:    total,
		progress: progress,
	}
	if progress != nil {
		progress(0, total)
	}
	if _, err := io.Copy(sink, resp.Body); err != nil {
		return "", sink.written, fmt.Errorf("下载中断（已收 %s）: %w", humanBytes(sink.written), err)
	}
	if err := file.Sync(); err != nil {
		return "", sink.written, fmt.Errorf("落盘 %s: %w", dest, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), sink.written, nil
}

// progressWriter 边写边报进度，并限频。
//
// 不限频的话，20MB 会产生上千次回调，每次都要跨 Wails 边界发一个事件 ——
// 前端被事件淹没，进度条反而更卡。
type progressWriter struct {
	w        io.Writer
	total    int64
	written  int64
	last     int64
	progress ProgressFunc
}

const progressStep = 512 * 1024

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if p.progress != nil && (p.written-p.last >= progressStep || p.written == p.total) {
		p.last = p.written
		p.progress(p.written, p.total)
	}
	return n, err
}

// extract 解压组件包到 dest，返回文件清单（含符号链接）。
//
// 组件包由我们的 CI 产出，但解压路径仍然全程校验：manifest 来自网络，
// 一个指向别处的包就能把文件写到任意位置。
func extract(ctx context.Context, zipPath, dest string) ([]fileEntry, error) {
	file, err := os.Open(zipPath)
	if err != nil {
		return nil, fmt.Errorf("打开组件包: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取组件包信息: %w", err)
	}
	zr, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, fmt.Errorf("组件包不是有效的 zip: %w", err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("创建解压目录: %w", err)
	}
	// 把解压目录先解析成真实路径，后面「链接落点是否在目录内」的判断才有意义。
	//
	// EvalSymlinks(link) 返回的是真实路径，而 dest 可能仍是别名：macOS 上
	// /var、/tmp 都是指向 /private/* 的符号链接，Linux 上也常见 /home → /data/home。
	// 不解析的话，包含关系恒为假 —— 组件包只要带一个合法链接，整包就装不上，
	// 报错还是「符号链接最终指向组件目录之外」，指向完全错误的方向。
	if resolved, err := filepath.EvalSymlinks(dest); err == nil {
		dest = resolved
	}

	prefix := TopLevelDir(zr.File)

	var (
		files    []fileEntry
		symlinks []pendingSymlink
	)
	for i, f := range zr.File {
		// 解压也要响应取消：31MB 上百个文件，用户点了取消不该还得等它写完。
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("安装已取消: %w", err)
			}
		}

		// 先剥顶层目录前缀，再去尾斜杠。顺序反过来会漏掉目录条目：
		// "scraper/" 去尾斜杠后是 "scraper"，它不以 "scraper/" 为前缀，
		// 于是被当成一个名字叫 scraper 的条目，把可执行文件的位置占了。
		rel := strings.TrimSuffix(strings.TrimPrefix(f.Name, prefix), "/")
		if rel == "" {
			continue
		}
		// 凭据文件由我们自己写。包内同名条目（尤其是符号链接）一律拒绝：
		// 一个指向 ../../ 的链接会让随后那次写凭据变成「写到组件目录之外」。
		if strings.EqualFold(rel, receiptFile) {
			return nil, fmt.Errorf("组件包内含有与安装凭据同名的条目：%s", rel)
		}

		target, err := safeJoin(dest, rel)
		if err != nil {
			return nil, err
		}

		mode := f.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, fmt.Errorf("创建目录 %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("创建目录 %s: %w", filepath.Dir(target), err)
		}

		if mode&os.ModeSymlink != 0 {
			// 链接目标要等常规文件就位后再建，否则指向尚未解出的文件会失败。
			link, err := readLink(f)
			if err != nil {
				return nil, err
			}
			if err := validateLinkTarget(rel, link); err != nil {
				return nil, err
			}
			symlinks = append(symlinks, pendingSymlink{path: target, target: link, rel: rel})
			continue
		}

		size, err := writeZipFile(f, target, mode)
		if err != nil {
			return nil, err
		}
		files = append(files, fileEntry{Path: filepath.ToSlash(rel), Size: size})
	}

	for _, link := range symlinks {
		if err := os.Symlink(link.target, link.path); err != nil {
			return nil, fmt.Errorf("创建符号链接 %s: %w", link.path, err)
		}
	}

	// 链接全部建好之后再统一核对最终落点。
	//
	// 建之前只能做词法判断，而词法判断会被「链接指向链接」绕过：
	// `a/up → ..` 与 `leak → a/up/../../victim` 单独看都在目录内，
	// 合起来 leak 的实际目标是组件根的上两级。只有让文件系统真正解析一遍
	// （EvalSymlinks）才能得到可信结果 —— 它同时还能发现成环与悬空链接。
	for _, link := range symlinks {
		real, err := filepath.EvalSymlinks(link.path)
		if err != nil {
			return nil, fmt.Errorf("符号链接 %s 无法解析（目标缺失或成环）: %w", link.rel, err)
		}
		if !withinDir(dest, real) {
			return nil, fmt.Errorf("符号链接 %s 最终指向组件目录之外：%s", link.rel, real)
		}
		// 链接也要进凭据：删掉一个用于加载动态库的链接，组件就不可运行了，
		// 而 Verify 只看文件清单的话会报「一切正常」，诊断直接把人带偏。
		files = append(files, fileEntry{Path: filepath.ToSlash(link.rel), Link: link.target})
	}
	return files, nil
}

// withinDir 判断 target 是否落在 base 之内（含 base 自身）。
func withinDir(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

type pendingSymlink struct {
	path   string
	target string
	// rel 是剥掉顶层目录后的包内相对路径，写凭据用。
	rel string
}

func readLink(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("读取符号链接 %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return "", fmt.Errorf("读取符号链接 %s: %w", f.Name, err)
	}
	return string(data), nil
}

// validateLinkTarget 确认链接不会指向解压目录之外。
//
// 三种拒绝理由，缺一不可：
//   - 绝对路径：Windows 上 `C:\` 形式经过 VolumeName 才算绝对，os.Symlink
//     会照单全收；
//   - 词法越界：`../../x` 从链接所在目录出发就能跳出组件目录，随后任何经过
//     该链接的写入都会落到外面；
//   - 指向安装凭据：凭据是解压之后才写的，若链接先占了那个位置，
//     `os.WriteFile` 会顺链接写到目标文件上。
func validateLinkTarget(rel, target string) error {
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return fmt.Errorf("组件包内含指向绝对路径的符号链接：%s → %s", rel, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(rel)), target))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("组件包内含越界的符号链接：%s → %s", rel, target)
	}
	if strings.EqualFold(resolved, receiptFile) {
		return fmt.Errorf("组件包内的符号链接指向安装凭据：%s → %s", rel, target)
	}
	return nil
}

func writeZipFile(f *zip.File, target string, mode os.FileMode) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("读取 %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return 0, fmt.Errorf("创建 %s: %w", target, err)
	}
	n, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("解压 %s: %w", f.Name, copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("写入 %s: %w", f.Name, closeErr)
	}
	return n, nil
}

// safeJoin 把包内路径拼到 dest 下，越界即报错。
func safeJoin(dest, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("组件包内含绝对路径：%s", name)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("组件包内含越界路径：%s", name)
	}
	target := filepath.Join(dest, clean)
	root := filepath.Clean(dest) + string(os.PathSeparator)
	if !strings.HasPrefix(target, root) {
		return "", fmt.Errorf("组件包内含越界路径：%s", name)
	}
	return target, nil
}

// TopLevelDir 返回包内所有条目的公共顶层目录（带尾部分隔符），没有则返回空。
//
// CI 用 `zip -r scraper.zip scraper` 打包，天然有一层 `scraper/`。不解掉这层，
// 组件目录里会多出一层同名目录，可执行文件路径与 toolpath 的约定对不上。
//
// 判断必须用原始条目名（保留目录条目尾部的 "/"）：`zip -r` 会先写入一条
// `scraper/` 目录条目，去掉尾斜杠后它就变成「没有分隔符的顶层裸条目」，
// 会被误判成「不存在公共目录」，于是整层目录被原样保留 —— 而文件条目仍然
// 正常解出，产物看上去「装好了」，只是路径多一层。
//
// 导出是为了让打包侧（cmd/manifestgen）用同一份规则校验发布包：那边如果自己
// 再写一遍「解压后会落成什么路径」，两份实现迟早分叉，而分叉的表现是「发布
// 成功、用户装完报缺少可执行文件」。
func TopLevelDir(files []*zip.File) string {
	top := ""
	for _, f := range files {
		idx := strings.IndexByte(f.Name, '/')
		if idx <= 0 {
			// 顶层裸文件（或空名字）：不存在公共目录。
			return ""
		}
		first := f.Name[:idx]
		if top != "" && top != first {
			return ""
		}
		top = first
	}
	if top == "" {
		return ""
	}
	return top + "/"
}

func containsFile(files []fileEntry, name string) bool {
	for _, f := range files {
		if f.Path == name && f.Link == "" {
			return true
		}
	}
	return false
}

func writeReceipt(dir string, rc receipt) error {
	data, err := json.MarshalIndent(rc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化安装凭据: %w", err)
	}
	path := filepath.Join(dir, receiptFile)
	// O_EXCL：凭据只能写进一个「原本什么都没有」的位置。前面已经拒绝了同名
	// 条目，这里再兜一层 —— 万一漏掉，宁可安装失败，也不能顺着一个已存在的
	// 符号链接把内容写到组件目录之外。
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("写入安装凭据 %s: %w", path, err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入安装凭据 %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("写入安装凭据 %s: %w", path, err)
	}
	return nil
}

// Remove 卸载组件：删掉指针条目与实际目录。
//
// 顺带清掉同名组件的其他版本目录 —— 用户点「移除」的语义是「把这块空间还给我」，
// 留下几个旧版本目录会让「移除后磁盘没变」变成一个需要解释的现象。
func (m *Manager) Remove(ctx context.Context, name string) error {
	if err := safeSegment("组件名", name); err != nil {
		return err
	}
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()

	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return fmt.Errorf("创建组件目录 %s: %w", m.root, err)
	}
	release, err := m.acquireLock(ctx, installLockWait)
	if err != nil {
		return err
	}
	defer release()

	var firstErr error
	activeRemoved := true
	if entry, ok := m.lookup(name); ok {
		dir := filepath.Join(m.root, entry.Dir)
		if err := os.RemoveAll(dir); err != nil {
			firstErr = fmt.Errorf("删除组件目录 %s 失败（可能仍有进程在使用它）：%w", dir, err)
			// 活动目录没删掉时不能动指针：指针是「这里装过什么」的唯一记录，
			// 删掉之后界面会变成「未安装」，而磁盘上留着几十 MB 文件，
			// 连「重试卸载」的入口都没了。
			activeRemoved = false
		}
	}

	// 其他版本目录尽力而为：删不掉不改变「已移除」这个结论，下次启动还会再试。
	if dirs, err := m.ownedDirs(name); err == nil {
		for _, dir := range dirs {
			if err := os.RemoveAll(dir); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("删除旧版本目录 %s 失败：%w", dir, err)
			}
		}
	}

	if activeRemoved {
		if err := m.dropPointer(name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Manager) dropPointer(name string) error {
	p, err := readPointer(m.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取组件指针: %w", err)
	}
	if _, ok := p.Components[name]; !ok {
		return nil
	}
	delete(p.Components, name)
	return writePointer(m.root, p)
}

// CleanupStale 删除半成品目录与不再被引用的旧版本目录。
//
// 在应用启动时调用一次。旧版本不在这里之外删除，是因为它可能正被一个还在跑的
// 刮削子进程使用（Windows 上删不掉，强删会中断刮削）。
//
// 三条例外，都是为了「宁可少删，不可错删」：
//   - 拿不到跨进程锁时整个跳过：另一个实例可能正在下载，它的 staging 目录
//     不能被当成遗留物删掉（删了就是「下载完成却报文件不存在」）；
//   - 指针不可读时只清半成品，不动版本目录：「哪些目录还有人用」是未知的，
//     猜错就是把用户刚下好的 20MB 删掉；
//   - 锁目录本身不参与清理。
func (m *Manager) CleanupStale(ctx context.Context) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()

	entries, err := os.ReadDir(m.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取组件目录 %s: %w", m.root, err)
	}

	release, err := m.acquireLock(ctx, 0)
	if err != nil {
		log.Printf("另一个实例正在安装组件，本次跳过清理")
		return nil
	}
	defer release()

	p, pointerErr := readPointer(m.root)
	referenced := map[string]bool{}
	for _, entry := range p.Components {
		referenced[entry.Dir] = true
	}
	if pointerErr != nil && !errors.Is(pointerErr, os.ErrNotExist) {
		log.Printf("警告：组件指针不可读（%v），本次只清理半成品目录", pointerErr)
	}

	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(m.root, name)

		if strings.HasPrefix(name, stagingPrefix) {
			if err := os.RemoveAll(path); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("清理半成品目录 %s: %w", path, err)
			}
			continue
		}
		if pointerErr != nil || referenced[name] {
			continue
		}
		if err := os.RemoveAll(path); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("清理旧版本目录 %s: %w", path, err)
		}
	}
	return firstErr
}

// ownedDirs 返回凭据里写明属于 name 的所有版本目录。
//
// 刻意不按目录名前缀匹配：组件名允许含连字符，`Remove("scraper")` 用前缀匹配
// 会连 `scraper-addon-1.0` 一起删掉 —— 而它的指针还在，那个组件就此损坏，
// 且症状是「另一个组件莫名消失」。读凭据确认归属，代价只是一次小文件读取。
//
// 凭据读不出来的目录不在这里删：归属未知，交给 CleanupStale 按指针引用情况
// 回收（那时它已经无人引用，删它不会伤到任何组件）。
func (m *Manager) ownedDirs(name string) ([]string, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), stagingPrefix) {
			continue
		}
		dir := filepath.Join(m.root, entry.Name())
		rc, err := readReceipt(dir)
		if err != nil {
			continue
		}
		if rc.Name == name {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// Verify 逐文件核对已安装组件与凭据记录是否一致，用于诊断与环境自检。
//
// 与 State 的分工：State 走的是「便宜且够用」的检查（目录在、可执行文件在、
// 协议对得上），每次启动都会用；Verify 会把文件清单逐个 stat 一遍（以及逐个
// 核对符号链接目标），只在用户主动排查与「幂等安装」时用。
func (m *Manager) Verify(ctx context.Context, name string) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	return m.verifyLocked(name)
}

// verifyLocked 是 Verify 的无锁版本，供已持有 m.mu 的调用方使用。
func (m *Manager) verifyLocked(name string) error {
	entry, ok := m.lookup(name)
	if !ok {
		return fmt.Errorf("组件 %s 未安装", name)
	}
	dir := filepath.Join(m.root, entry.Dir)
	rc, err := readReceipt(dir)
	if err != nil {
		return fmt.Errorf("组件凭据不可读: %w", err)
	}
	if len(rc.Files) == 0 {
		return errors.New("组件凭据里没有文件清单")
	}

	var missing, mismatched int
	for _, f := range rc.Files {
		path := filepath.Join(dir, filepath.FromSlash(f.Path))
		if f.Link != "" {
			info, err := os.Lstat(path)
			if err != nil {
				missing++
				continue
			}
			if info.Mode()&os.ModeSymlink == 0 {
				mismatched++
				continue
			}
			target, err := os.Readlink(path)
			if err != nil || target != f.Link {
				mismatched++
			}
			continue
		}
		info, err := os.Stat(path)
		switch {
		case err != nil:
			missing++
		case info.Size() != f.Size:
			mismatched++
		case f.Path == exeName(rc.Binary) && !hasExecBit(info):
			// 可执行位丢了也算损坏：exec 会 permission denied，
			// 而「文件都在、大小也对」看上去完全正常。
			mismatched++
		}
	}
	if missing > 0 || mismatched > 0 {
		return fmt.Errorf("组件文件不完整：缺少 %d 个，与凭据不符 %d 个（共 %d 个）", missing, mismatched, len(rc.Files))
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
