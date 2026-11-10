// Package locate 负责在运行时定位外部可执行文件。
//
// 查找顺序：
//  1. 配置里写死的路径（用户显式指定，错了就报错，不静默回退）
//  2. 主程序同级目录及其 bin/ 子目录（Windows/裸二进制发行）
//  3. 若主程序在 X.app/Contents/MacOS 内，则再往上找 X.app 所在目录及其 bin/
//     （macOS 发行时把三件套平铺在 .app 旁边）
//  4. 工作目录及其上级（开发模式）
//  5. 系统 PATH
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

// Dlr 定位 N_m3u8DL-RE，返回路径与来源说明（用于 UI 展示）。
func Dlr(cfg config.Config) (string, string, error) {
	if p := strings.TrimSpace(cfg.DlrPath); p != "" {
		if executableFile(p) {
			return p, "配置指定", nil
		}
		return "", "", fmt.Errorf("配置中的路径不可执行: %s", p)
	}

	if p, ok := findExecutable(exeName("N_m3u8DL-RE")); ok {
		return p, "程序同级目录", nil
	}
	if p, err := exec.LookPath(exeName("N_m3u8DL-RE")); err == nil {
		return p, "系统 PATH", nil
	}
	return "", "", errors.New("未找到 N_m3u8DL-RE，请在设置中指定可执行文件路径")
}

// Scrape 定位爬取入口，返回可直接 exec 的参数列表与工作目录。
//
// 优先级：
//  1. 配置里的 PythonCmd（可带参数，如 "uv run jab-scrape"）
//  2. 发行目录里的 jab-scrape 可执行文件（同级或 python/ 子目录）
//  3. 开发模式：向上找到含 python/pyproject.toml 的目录，用 uv run --project
func Scrape(cfg config.Config) (argv []string, dir string, err error) {
	if raw := strings.TrimSpace(cfg.PythonCmd); raw != "" {
		argv = splitCommand(raw)
		if len(argv) == 0 {
			return nil, "", errors.New("爬取命令为空")
		}
		return argv, workDirFor(argv[0]), nil
	}

	for _, rel := range []string{
		exeName("jab-scrape"),
		filepath.Join("bin", exeName("jab-scrape")),
		filepath.Join("python", exeName("jab-scrape")),
	} {
		if p, ok := findExecutable(rel); ok {
			return []string{p}, filepath.Dir(p), nil
		}
	}

	if proj, ok := findPythonProject(); ok {
		if _, e := exec.LookPath("uv"); e == nil {
			return []string{"uv", "run", "--project", proj, "jab-scrape"}, proj, nil
		}
		return nil, "", errors.New("开发模式需要 uv 才能运行 jab-scrape")
	}

	return nil, "", errors.New("未找到爬取程序（jab-scrape），请在设置中指定命令")
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
