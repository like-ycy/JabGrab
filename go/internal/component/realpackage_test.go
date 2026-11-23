package component

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallRealPackage 用真实的发布包跑一遍安装。
//
// 合成 zip 能覆盖逻辑，但覆盖不了「CI 实际产出的包长什么样」：目录条目顺序、
// 可执行位、_internal 的层级、20MB 量级下的进度回调。这一步在打包脚本改动后
// 值得手动跑一次：
//
//	cd python/build/pyi/dist && zip -qr /tmp/scraper.zip jab-scrape   # 或直接取 CI 产物
//	JABGRAB_TEST_COMPONENT_ZIP=/tmp/scraper.zip go test ./internal/component -run RealPackage -v
//
// 默认跳过：它依赖仓库外的文件，不该成为 go test 的固定负担。
func TestInstallRealPackage(t *testing.T) {
	zipPath := os.Getenv("JABGRAB_TEST_COMPONENT_ZIP")
	if zipPath == "" {
		t.Skip("未设置 JABGRAB_TEST_COMPONENT_ZIP，跳过真实包安装测试")
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("读取 %s: %v", zipPath, err)
	}

	root := testDir(t)
	m := New(root)
	spec, _ := serve(t, data)
	// 真实包里的可执行文件叫 jab-scrape（组件名是 scraper），必须显式指明，
	// 否则安装会以「包内缺少可执行文件 scraper」失败。
	spec.Binary = "jab-scrape"
	spec.Version = "0.1.0"
	path := mustInstall(t, m, spec)

	t.Logf("安装到 %s（包 %d 字节）", path, len(data))
	if got := m.State(spec); got != Ready {
		t.Fatalf("状态应为 Ready，实际 %s", got)
	}
	if err := m.Verify(context.Background(), "scraper"); err != nil {
		t.Fatalf("真实包校验失败: %v", err)
	}

	// 真跑一次：能输出 --version 才说明可执行位、_internal 层级、动态库路径都对。
	// 这里刻意用 --version 而不是 --help：后者只走 argparse，前者连 seleniumbase
	// 都不碰，两者都不足以证明「模块都打进去了」（见 python/src/jab_scrape/__main__.py
	// 的 _selftest 注释），所以再跑一次 --selftest。
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatalf("运行安装后的可执行文件失败: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("--version 没有输出")
	}
	if self, err := exec.Command(path, "--selftest").CombinedOutput(); err != nil {
		t.Fatalf("--selftest 失败: %v\n%s", err, self)
	}
	t.Logf("version 输出：%s", strings.TrimSpace(string(out)))

	// 安装目录结构：可执行文件与 _internal 同级。
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "_internal")); err != nil {
		t.Fatalf("_internal 不在可执行文件同级: %v", err)
	}
}
