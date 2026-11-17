package component

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStateMissingOnEmptyRoot 确认没装过就是 Missing。
func TestStateMissingOnEmptyRoot(t *testing.T) {
	m := New(testDir(t))
	spec := Spec{Name: "scraper", Version: "0.4.3", Protocol: 1}
	if got := m.State(spec); got != Missing {
		t.Fatalf("期望 Missing，实际 %s", got)
	}
	if got := m.Path("scraper"); got != "" {
		t.Fatalf("未安装时 Path 应返回空，实际 %q", got)
	}
}

// TestStateOutdatedWhenVersionDiffers 确认已装版本与期望不同时报 Outdated。
//
// 这一档与 Ready 分开，是为了让 UI 能显示「可更新」而不是「未安装」——
// 后者会诱导用户重下一遍同样的东西。
func TestStateOutdatedWhenVersionDiffers(t *testing.T) {
	m := New(testDir(t))
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	newer := spec
	newer.Version = "0.4.4"
	if got := m.State(newer); got != Outdated {
		t.Fatalf("期望 Outdated，实际 %s", got)
	}
}

// TestStateIncompatibleWhenProtocolDiffers 确认协议不匹配优先于版本判断。
//
// 协议不匹配时升级组件也未必能用，用户需要的是「升级 App」而不是「更新组件」，
// 两档混在一起会把引导指错方向。
func TestStateIncompatibleWhenProtocolDiffers(t *testing.T) {
	m := New(testDir(t))
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	other := spec
	other.Version = "0.5.0" // 版本也不同，但应报 Incompatible
	other.Protocol = 2
	if got := m.State(other); got != Incompatible {
		t.Fatalf("期望 Incompatible，实际 %s", got)
	}
}

// TestStateCorruptWhenBinaryMissing 确认可执行文件被删后不再报 Ready。
func TestStateCorruptWhenBinaryMissing(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	path := mustInstall(t, m, spec)

	if err := os.Remove(path); err != nil {
		t.Fatalf("删除可执行文件: %v", err)
	}
	if got := m.State(spec); got != Corrupt {
		t.Fatalf("期望 Corrupt，实际 %s", got)
	}
}

// TestStateCorruptWhenReceiptBroken 确认凭据读不出来时是 Corrupt 而不是 Missing。
//
// 报 Missing 会让 UI 显示「未安装」，而磁盘上还留着几十 MB 垃圾，
// 用户点「移除」也找不到东西可移。
func TestStateCorruptWhenReceiptBroken(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	receipt := filepath.Join(root, "scraper-0.4.3", receiptFile)
	if err := os.WriteFile(receipt, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("破坏凭据: %v", err)
	}
	if got := m.State(spec); got != Corrupt {
		t.Fatalf("期望 Corrupt，实际 %s", got)
	}
}

// TestRemoveDeletesEverything 确认移除后目录与指针都干净。
func TestRemoveDeletesEverything(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	if err := m.Remove("scraper"); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	if got := m.State(spec); got != Missing {
		t.Fatalf("移除后应为 Missing，实际 %s", got)
	}
	if dirs, err := m.ownedDirs("scraper"); err != nil || len(dirs) != 0 {
		t.Fatalf("移除后仍有版本目录：%v（err=%v）", dirs, err)
	}
	if _, err := readPointer(root); err != nil {
		t.Fatalf("指针文件应仍然可读: %v", err)
	}
}

// TestCleanupStaleRemovesOldVersionsAndStaging 确认旧版本与半成品被回收。
//
// 旧版本不能随手删：它可能正被一个还在跑的刮削子进程占用（Windows 上删不掉）。
// 但安装成功后留下的旧版本没有任何引用，必须由启动时的清理收回空间，
// 否则每升一次版本就多留 31MB。
func TestCleanupStaleRemovesOldVersionsAndStaging(t *testing.T) {
	root := testDir(t)
	m := New(root)
	old, _ := serve(t, standardPackage(t))
	mustInstall(t, m, old)

	newer := old
	newer.Version = "0.4.4"
	mustInstall(t, m, newer)

	staging := filepath.Join(root, stagingPrefix+"123")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("造半成品目录: %v", err)
	}

	if err := m.CleanupStale(); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "scraper-0.4.3")); !os.IsNotExist(err) {
		t.Fatalf("旧版本目录未被清理：%v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("半成品目录未被清理：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "scraper-0.4.4")); err != nil {
		t.Fatalf("当前版本目录被误删：%v", err)
	}
}

// TestCleanupStaleKeepsVersionsWhenPointerUnreadable 确认指针坏掉时不乱删。
//
// 这种情况下「哪些目录还有人用」是未知的，猜错就是把用户刚下好的组件删掉。
// 只清半成品是唯一安全的做法。
func TestCleanupStaleKeepsVersionsWhenPointerUnreadable(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	if err := os.WriteFile(filepath.Join(root, pointerFile), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("破坏指针: %v", err)
	}
	staging := filepath.Join(root, stagingPrefix+"456")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("造半成品目录: %v", err)
	}
	if err := m.CleanupStale(); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "scraper-0.4.3")); err != nil {
		t.Fatalf("指针不可读时误删了版本目录：%v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("半成品目录应被清理：%v", err)
	}
}

// TestVerifyDetectsTruncatedFile 确认诊断能发现文件被动过。
//
// State 只看目录与可执行文件（每次启动都要跑，不能逐文件 stat 31MB）；
// 真正的完整性靠 Verify，用于环境诊断。
func TestVerifyDetectsTruncatedFile(t *testing.T) {
	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, standardPackage(t))
	mustInstall(t, m, spec)

	if err := m.Verify("scraper"); err != nil {
		t.Fatalf("刚装好的组件不应报错：%v", err)
	}

	target := filepath.Join(root, "scraper-0.4.3", "_internal", "libpython.dylib")
	if err := os.WriteFile(target, []byte("short"), 0o644); err != nil {
		t.Fatalf("截断文件: %v", err)
	}
	err := m.Verify("scraper")
	if err == nil {
		t.Fatal("文件被截断却校验通过")
	}
	if !strings.Contains(err.Error(), "文件不完整") {
		t.Fatalf("错误信息未指出不完整：%v", err)
	}
}

// TestSpecValidate 逐项确认会拼进路径的字段与下载信息都被校验。
//
// 这些字段都可能来自网络上的 manifest，放过任何一个都意味着目录穿越或
// 「下完 20MB 才发现校验码写错了」。
func TestSpecValidate(t *testing.T) {
	valid := Spec{
		Name:     "scraper",
		Version:  "0.4.3",
		Protocol: 1,
		Asset:    Asset{URL: "https://example.com/a.zip", Size: 10, SHA256: strings.Repeat("a", 64)},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 Spec 被拒绝: %v", err)
	}

	cases := map[string]func(s *Spec){
		"组件名为空":     func(s *Spec) { s.Name = "" },
		"组件名含斜杠":    func(s *Spec) { s.Name = "a/b" },
		"组件名是点点":    func(s *Spec) { s.Name = ".." },
		"版本号含分隔符":   func(s *Spec) { s.Version = `a\b` },
		"可执行文件名含斜杠": func(s *Spec) { s.Binary = "a/b" },
		"下载地址为空":    func(s *Spec) { s.Asset.URL = "" },
		"下载地址协议不支持": func(s *Spec) { s.Asset.URL = "ftp://example.com/a.zip" },
		"校验码长度不对":   func(s *Spec) { s.Asset.SHA256 = "abc" },
		"校验码含非十六进制": func(s *Spec) { s.Asset.SHA256 = strings.Repeat("g", 64) },
		"包大小为负":     func(s *Spec) { s.Asset.Size = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := valid
			mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatalf("%s 却通过了校验：%+v", name, spec)
			}
		})
	}
}
