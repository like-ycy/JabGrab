package component

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInstallRejectsEscapingSymlink 确认相对符号链接也不能越出组件目录。
//
// 只拦绝对路径是不够的：`../../victim` 从链接所在目录出发同样能跳出去，
// 而它看上去只是包里的一条普通条目。
func TestInstallRejectsEscapingSymlink(t *testing.T) {
	root := testDir(t)
	m := New(root)
	payload := buildZip(t, []zipEntry{
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "scraper/_internal/evil", body: "../../../../victim.json", mode: os.ModeSymlink | 0o777},
	})
	spec, _ := serve(t, payload)

	_, err := m.Install(t.Context(), spec, nil)
	if err == nil {
		t.Fatal("越界符号链接未被拒绝")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Fatalf("错误信息未指出越界：%v", err)
	}
	assertNoResidue(t, root)
}

// TestInstallRejectsReceiptSymlink 确认包内条目不能占用安装凭据的位置。
//
// 凭据是解压之后才写的。若包里有一条指向外部的 `component.json` 链接，
// 那次写入会顺着链接落到组件目录之外 —— 攻击者用一个「看起来正常」的包
// 就能覆盖用户机器上的任意文件。
func TestInstallRejectsReceiptSymlink(t *testing.T) {
	t.Run("同名条目", func(t *testing.T) {
		root := testDir(t)
		m := New(root)
		payload := buildZip(t, []zipEntry{
			{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
			{name: "scraper/" + receiptFile, body: "../../../../victim.json", mode: os.ModeSymlink | 0o777},
		})
		spec, _ := serve(t, payload)

		if _, err := m.Install(t.Context(), spec, nil); err == nil {
			t.Fatal("与凭据同名的条目未被拒绝")
		} else if !strings.Contains(err.Error(), "安装凭据") {
			t.Fatalf("错误信息未指出凭据冲突：%v", err)
		}
		assertNoResidue(t, root)
	})

	t.Run("链接指向凭据", func(t *testing.T) {
		root := testDir(t)
		m := New(root)
		payload := buildZip(t, []zipEntry{
			{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
			{name: "scraper/link", body: receiptFile, mode: os.ModeSymlink | 0o777},
		})
		spec, _ := serve(t, payload)

		if _, err := m.Install(t.Context(), spec, nil); err == nil {
			t.Fatal("指向安装凭据的链接未被拒绝")
		} else if !strings.Contains(err.Error(), "安装凭据") {
			t.Fatalf("错误信息未指出凭据冲突：%v", err)
		}
		assertNoResidue(t, root)
	})
}

// TestInstallRepairsMissingFile 确认「状态 Ready 但文件缺失」能靠重装自愈。
//
// State 只检查可执行文件（每次启动都要调，不能逐文件 stat）。动态库被删或
// 被杀软隔离时主程序还在，State 仍是 Ready —— 若幂等分支直接返回，用户就
// 再也装不回来了：刮削每次失败，点重装没有任何效果。
func TestInstallRepairsMissingFile(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	dylib := filepath.Join(root, "scraper-0.4.3", "_internal", "libpython.dylib")
	if err := os.Remove(dylib); err != nil {
		t.Fatalf("删除附属文件: %v", err)
	}
	if got := m.State(spec); got != Ready {
		t.Fatalf("前置条件不成立：State = %s，期望 Ready（只查可执行文件）", got)
	}

	mustInstall(t, m, spec)
	if _, err := os.Stat(dylib); err != nil {
		t.Fatalf("重装后附属文件仍未恢复: %v", err)
	}
	if err := m.Verify(context.Background(), "scraper"); err != nil {
		t.Fatalf("重装后完整性仍不通过: %v", err)
	}
}

// TestVerifyDetectsMissingSymlink 确认符号链接被删也能查出来。
//
// 用于加载动态库的链接一旦消失，组件就跑不起来，而文件清单里若不含链接，
// 诊断会报「一切正常」，把人直接带偏。
func TestVerifyDetectsMissingSymlink(t *testing.T) {
	if exeName("x") == "x.exe" {
		t.Skip("Windows 创建符号链接需要开发者模式或管理员权限")
	}
	root := testDir(t)
	m := New(root)
	payload := buildZip(t, []zipEntry{
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "scraper/_internal/lib.dylib", body: "lib", mode: 0o644},
		{name: "scraper/lib.dylib", body: "_internal/lib.dylib", mode: os.ModeSymlink | 0o777},
	})
	spec, _ := serve(t, payload)
	mustInstall(t, m, spec)

	if err := m.Verify(context.Background(), "scraper"); err != nil {
		t.Fatalf("刚装好的组件不应报错：%v", err)
	}
	if err := os.Remove(filepath.Join(root, "scraper-0.4.3", "lib.dylib")); err != nil {
		t.Fatalf("删除符号链接: %v", err)
	}
	if err := m.Verify(context.Background(), "scraper"); err == nil {
		t.Fatal("符号链接被删却校验通过")
	}
}

// TestRemoveKeepsSiblingComponent 确认卸载不会误伤名字带前缀的其他组件。
//
// 组件名允许含连字符。按目录名前缀匹配时，`Remove("scraper")` 会连
// `scraper-addon-1.0` 一起删掉，而后者指针还在 —— 那个组件就此损坏，
// 用户看到的是「另一个组件莫名消失」。
func TestRemoveKeepsSiblingComponent(t *testing.T) {
	root := testDir(t)
	m := New(root)

	base, _ := serve(t, standardPackage(t))
	mustInstall(t, m, base)

	addonData := buildZip(t, []zipEntry{
		{name: "addon/" + exeName("addon"), body: "x", mode: 0o755},
	})
	addonSpec, _ := serve(t, addonData)
	addonSpec.Name = "scraper-addon"
	addonSpec.Binary = "addon"
	mustInstall(t, m, addonSpec)

	if err := m.Remove(context.Background(), "scraper"); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	if got := m.State(addonSpec); got != Ready {
		t.Fatalf("兄弟组件被误删：State = %s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "scraper-addon-0.4.3")); err != nil {
		t.Fatalf("兄弟组件目录被误删: %v", err)
	}
}

// TestCleanupStaleSkipsWhileAnotherInstanceInstalling 确认清理不会删掉另一个
// 实例正在用的 staging 目录。
//
// 两个 App 实例共用数据目录时，B 的启动清理会把 A 正在下载的 staging 当遗留物
// 删掉，A 的表现是「下载完成却报 payload.zip 不存在」—— 与网络、磁盘都无关，
// 现场看不出任何线索。
func TestCleanupStaleSkipsWhileAnotherInstanceInstalling(t *testing.T) {
	root := testDir(t)

	payload := standardPackage(t)
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先发头再挂住：模拟「正在下载」的状态，长度未知（chunked），
		// 客户端按 spec 里的大小报进度。
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-release
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	spec := Spec{
		Name:     "scraper",
		Version:  "0.4.3",
		Protocol: 1,
		Asset:    Asset{URL: srv.URL, Size: int64(len(payload)), SHA256: sha256Hex(payload)},
	}

	installer := New(root)
	done := make(chan error, 1)
	go func() {
		_, err := installer.Install(context.Background(), spec, nil)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("下载未开始")
	}

	// 另一个实例在安装进行中做启动清理。
	other := New(root)
	if err := other.CleanupStale(context.Background()); err != nil {
		t.Fatalf("清理不该报错（拿不到锁时应跳过）：%v", err)
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("安装被另一实例的清理打断: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("安装超时")
	}
	if got := installer.State(spec); got != Ready {
		t.Fatalf("安装后状态应为 Ready，实际 %s", got)
	}
}

// TestInstallRejectsSymlinkChainEscape 确认「链接指向链接」也逃不出去。
//
// 词法检查挡不住这种组合：`a/up → ..` 与 `leak → a/up/../../victim` 单看都在
// 目录内，合起来的实际目标却是组件根的上两级。必须让文件系统真解析一遍。
func TestInstallRejectsSymlinkChainEscape(t *testing.T) {
	root := testDir(t)
	m := New(root)

	victim := filepath.Join(filepath.Dir(root), "victim-chain.txt")
	if err := os.WriteFile(victim, []byte("secret"), 0o644); err != nil {
		t.Fatalf("造外部文件: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(victim) })

	payload := buildZip(t, []zipEntry{
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "scraper/a/up", body: "..", mode: os.ModeSymlink | 0o777},
		{name: "scraper/leak", body: "a/up/../../victim-chain.txt", mode: os.ModeSymlink | 0o777},
	})
	spec, _ := serve(t, payload)

	_, err := m.Install(t.Context(), spec, nil)
	if err == nil {
		t.Fatal("链式符号链接越界未被拒绝")
	}
	if !strings.Contains(err.Error(), "组件目录之外") && !strings.Contains(err.Error(), "无法解析") {
		t.Fatalf("错误信息未指出越界：%v", err)
	}
	assertNoResidue(t, root)
}

// TestStateDetectsMissingExecBit 确认可执行位丢失能被发现并靠重装修复。
//
// macOS/Linux 上权限被改成 0644 时 exec 直接 permission denied，而文件都在、
// 大小也对 —— 只看「是普通文件」的话 State 报 Ready，用户点重装也不会有任何效果。
func TestStateDetectsMissingExecBit(t *testing.T) {
	if exeName("x") == "x.exe" {
		t.Skip("Windows 可执行性由扩展名决定，没有权限位")
	}
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	path := mustInstall(t, m, spec)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("改权限: %v", err)
	}
	if got := m.State(spec); got != Corrupt {
		t.Fatalf("可执行位丢失应为 Corrupt，实际 %s", got)
	}
	if err := m.Verify(context.Background(), "scraper"); err == nil {
		t.Fatal("可执行位丢失却校验通过")
	}

	mustInstall(t, m, spec)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("重装后可执行位仍未恢复：%v", info.Mode())
	}
}

// TestLockIsExclusive 确认同一根目录同一时刻只有一个持有者。
//
// 这是「B 的启动清理删掉 A 正在下载的 staging」的根因所在，所以锁本身
// 也要单独钉住，而不是只依赖上层的行为测试。
func TestLockIsExclusive(t *testing.T) {
	root := testDir(t)
	first := New(root)
	second := New(root)

	release, err := first.acquireLock(t.Context(), 0)
	if err != nil {
		t.Fatalf("第一个实例应拿到锁: %v", err)
	}
	if _, err := second.acquireLock(t.Context(), 0); err == nil {
		t.Fatal("第二个实例也拿到了锁")
	}

	release()
	release2, err := second.acquireLock(t.Context(), 0)
	if err != nil {
		t.Fatalf("释放后第二个实例应拿到锁: %v", err)
	}
	release2()
}

// 取消发生在最后一次进度回调时，此时校验已过。若不检查 ctx，后续解压、
// 替换目录、写指针都会照做 —— 用户以为取消了，实际下次启动用的就是新版本。
func TestInstallAbortsWhenCancelledAtLastByte(t *testing.T) {
	root := testDir(t)
	m := New(root)
	data := standardPackage(t)
	spec, _ := serve(t, data)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, err := m.Install(ctx, spec, func(done, total int64) {
		if done == total && total > 0 {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("取消后安装仍成功")
	}
	assertNoResidue(t, root)
	if got := m.State(spec); got != Missing {
		t.Fatalf("取消后不应留下可用安装：State = %s", got)
	}
}

// TestInstallWithSymlinkedRoot 确认「组件根目录本身是符号链接」时，带链接的包也能装上。
//
// 修之前这里必挂：EvalSymlinks(链接) 返回真实路径，而 dest 仍是别名，
// withinDir 恒为假 —— 报错却是「符号链接最终指向组件目录之外」，把人引向
// 安全校验而不是路径解析。macOS 的 /var、/tmp 与部分 Linux 的 /home 都可能
// 落在这种链路上。
func TestInstallWithSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要开发者模式或管理员权限")
	}
	realRoot := testDir(t)
	alias := realRoot + "-alias"
	// 链接目标必须是绝对路径：testDir 返回相对路径，相对目标会按「链接所在
	// 目录」解析，两个 ../ 之后就指到仓库外了，得到一条悬空链接。
	abs, err := filepath.Abs(realRoot)
	if err != nil {
		t.Fatalf("取绝对路径: %v", err)
	}
	if err := os.Symlink(abs, alias); err != nil {
		t.Fatalf("创建别名目录: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })

	m := New(alias)
	data := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o755},
		{name: "scraper/_internal/lib.dylib", body: "lib", mode: 0o644},
		{name: "scraper/lib.dylib", body: "_internal/lib.dylib", mode: os.ModeSymlink | 0o777},
	})
	spec, _ := serve(t, data)
	mustInstall(t, m, spec)

	link := filepath.Join(realRoot, "scraper-0.4.3", "lib.dylib")
	target, err := os.Readlink(link)
	if err != nil || target != "_internal/lib.dylib" {
		t.Fatalf("链接目标 = %q（%v）", target, err)
	}
}

// TestInstallRejectsNonExecutableBinary 确认包内主程序没有可执行位时不装。
//
// 修之前只核对文件名：装完之后 State 报 Corrupt，而重装会命中幂等分支直接
// 返回，用户永远修不好，看到的却是「安装成功」。
func TestInstallRejectsNonExecutableBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的可执行性由扩展名决定，没有权限位")
	}
	root := testDir(t)
	m := New(root)
	data := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "x", mode: 0o644},
	})
	spec, _ := serve(t, data)

	if _, err := m.Install(t.Context(), spec, nil); err == nil {
		t.Fatal("无执行权限的包不应安装成功")
	}
	assertNoResidue(t, root)
	if got := m.State(spec); got != Missing {
		t.Fatalf("State = %s，期望 missing", got)
	}
}

// TestInstallReplacesPackageWithNewChecksum 确认「同版本号、内容变了」的包会被替换。
//
// 发布流程用 `gh release upload --clobber` 覆盖资产，版本号常常不变。修之前
// 幂等分支只比版本与文件大小，用户手里那份旧包永远不会被换掉，而界面上显示
// 「已是最新」。
func TestInstallReplacesPackageWithNewChecksum(t *testing.T) {
	root := testDir(t)
	m := New(root)

	oldPkg := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "v1", mode: 0o755},
	})
	newPkg := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "v2-changed", mode: 0o755},
	})
	oldSpec, _ := serve(t, oldPkg)
	newSpec, hits := serve(t, newPkg)
	// 版本与协议刻意保持一致：变的只有包内容。
	newSpec.Name, newSpec.Version, newSpec.Protocol = oldSpec.Name, oldSpec.Version, oldSpec.Protocol

	path := mustInstall(t, m, oldSpec)
	if got := readFileString(t, path); got != "v1" {
		t.Fatalf("首次安装内容 = %q", got)
	}

	path2 := mustInstall(t, m, newSpec)
	if *hits == 0 {
		t.Fatal("校验码变化后应当重新下载，实际一个请求都没发")
	}
	if got := readFileString(t, path2); got != "v2-changed" {
		t.Fatalf("重装后内容 = %q，期望 v2-changed", got)
	}

	// 旧目录是被「先 rename 到 .old-* 再删」的，不该留下残留。
	// 留着的话 CleanupStale 会回收，但说明删那一步失败了 —— 在测试里
	// 它应该总成功，失败意味着 replaceDir 的清理分支没生效。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读取组件目录: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".old-") {
			t.Fatalf("替换后残留旧目录：%s", entry.Name())
		}
	}
}

// TestPathRejectsNonExecutable 确认 Path 的契约是「不可用返回空」。
//
// 权限被改回 0644（备份还原、杀软、解压工具都可能这么干）时文件还在，
// 但 exec 会 permission denied。返回非空路径等于把「跑不起来」伪装成
// 「装好了」—— State 那边报了 Corrupt，这里却给出一个能用的路径，
// 调用方会选哪个是不确定的。
func TestPathRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的可执行性由扩展名决定，没有权限位")
	}
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	path := mustInstall(t, m, spec)

	if got := m.Path(spec.Name); got != path {
		t.Fatalf("Path = %q，期望 %q", got, path)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("改权限: %v", err)
	}
	if got := m.Path(spec.Name); got != "" {
		t.Fatalf("权限丢失后 Path 仍返回 %q", got)
	}
}

// TestInstallCancelsWhileWaitingInProcess 确认「排队等前一次安装」时能响应取消。
//
// 一次安装可能在下载阶段停留十几分钟（20MB 慢网）。用 sync.Mutex 的话，第二个
// 调用既不能取消也不能超时，用户点了「取消」界面毫无反应 —— 而跨进程那把锁
// 反而是支持 ctx 的，两个层次的行为不该不一致。
func TestInstallCancelsWhileWaitingInProcess(t *testing.T) {
	root := testDir(t)
	m := New(root)
	payload := standardPackage(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	// 直接指向上面那个会阻塞的 server（serve 建的是另一个，用它永远等不到 started）。
	spec := Spec{
		Name:     "scraper",
		Version:  "0.4.3",
		Protocol: 1,
		Asset: Asset{
			URL:    srv.URL + "/scraper.zip",
			Size:   int64(len(payload)),
			SHA256: sha256Hex(payload),
		},
	}

	firstDone := make(chan error, 1)
	go func() {
		// 用同一份 payload 的另一个 server 也行，这里直接用 spec（它指向 serve 的 server）。
		_, err := m.Install(t.Context(), spec, nil)
		firstDone <- err
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一个安装没有开始下载")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 已经取消：应当立刻失败，而不是排队等下载结束

	waitDone := make(chan error, 1)
	go func() {
		_, err := m.Install(ctx, spec, nil)
		waitDone <- err
	}()

	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatal("已取消的请求不该成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待进程内锁时没有响应取消")
	}

	unblock()
	if err := <-firstDone; err != nil {
		t.Fatalf("第一个安装应正常完成: %v", err)
	}
}

// TestInstallRollsBackWhenPointerWriteFails 确认「指针写不进去」时旧安装原样保留。
//
// 这是「同版本重装」才会走到的路径：目标目录正是指针指向的那份安装。修复前
// 它会先被删掉，于是 Install 报告失败，而磁盘上既没有旧版本也没有生效的新版本。
func TestInstallRollsBackWhenPointerWriteFails(t *testing.T) {
	root := testDir(t)
	m := New(root)

	oldPkg := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "v1", mode: 0o755},
	})
	newPkg := buildZip(t, []zipEntry{
		{name: "scraper/", mode: os.ModeDir | 0o755},
		{name: "scraper/" + exeName("scraper"), body: "v2-changed", mode: 0o755},
	})
	oldSpec, _ := serve(t, oldPkg)
	newSpec, _ := serve(t, newPkg)
	newSpec.Name, newSpec.Version, newSpec.Protocol = oldSpec.Name, oldSpec.Version, oldSpec.Protocol

	path := mustInstall(t, m, oldSpec)
	if got := readFileString(t, path); got != "v1" {
		t.Fatalf("首次安装内容 = %q", got)
	}

	// 让指针写入失败：writePointer 先写 installed.json.tmp 再 rename。
	// 在那个位置放一个目录，os.WriteFile 立刻报「是个目录」。
	if err := os.Mkdir(filepath.Join(root, pointerFile+".tmp"), 0o755); err != nil {
		t.Fatalf("造阻塞: %v", err)
	}

	if _, err := m.Install(t.Context(), newSpec, nil); err == nil {
		t.Fatal("指针写不进去时不该报告成功")
	}

	if got := readFileString(t, path); got != "v1" {
		t.Fatalf("回滚后内容 = %q，期望 v1", got)
	}
	if got := m.State(oldSpec); got != Ready {
		t.Fatalf("回滚后 State = %s，期望 ready", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读取组件目录: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".old-") {
			t.Fatalf("回滚后残留旧目录：%s", entry.Name())
		}
	}
}

// TestRemoveKeepsPointerWhenActiveDirUndeletable 确认「删不掉」时不会假装删掉了。
//
// 修之前：目录删失败、指针照删，界面变成「未安装」，磁盘上留着几十 MB，
// 而「重新卸载」的入口已经消失 —— 用户只能自己去翻数据目录。
func TestRemoveKeepsPointerWhenActiveDirUndeletable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的目录权限模型不同，这里用 chmod 模拟")
	}
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	// 去掉活动目录的写权限：删不掉里面的条目。
	active := filepath.Join(root, spec.Name+"-"+spec.Version)
	if err := os.Chmod(active, 0o500); err != nil {
		t.Fatalf("收紧权限: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(active, 0o755) })

	if err := m.Remove(context.Background(), spec.Name); err == nil {
		t.Fatal("删不掉时不该返回成功")
	}
	if _, _, ok := m.Installed(spec.Name); !ok {
		t.Fatal("删不掉时不该丢掉安装记录（否则重试入口也没了）")
	}
	if got := m.LocalState(spec.Name, spec.Protocol); got != Ready {
		t.Fatalf("文件仍然完整可用，LocalState 应为 ready，实际 %s", got)
	}

	// 恢复权限后重试应当成功。
	if err := os.Chmod(active, 0o755); err != nil {
		t.Fatalf("恢复权限: %v", err)
	}
	if err := m.Remove(context.Background(), spec.Name); err != nil {
		t.Fatalf("恢复权限后卸载: %v", err)
	}
	if _, _, ok := m.Installed(spec.Name); ok {
		t.Fatal("卸载成功后不该还有记录")
	}
}
