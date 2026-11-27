package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PruneResult 是清理 txt 的结果，直接回给前端写日志。
type PruneResult struct {
	// Removed 是从文件里删掉的 URL 行数。
	Removed int `json:"removed"`
	// Kept 是文件里剩下的 URL 行数（空行与 # 注释不计）。
	Kept int `json:"kept"`
	// Path 是本次被改写的文件。
	Path string `json:"path"`
}

// Downloaded 报告这个状态是否算「这条 URL 已经下好了」。
//
// 完成当然算；已存在（以前下过、这次跳过）也算——留着它下次还是会被跳过，
// 对用户没有任何用处，删掉才是他想要的清单。
func Downloaded(st Status) bool {
	return st == StatusDone || st == StatusSkipped
}

// PruneURLs 把 urls 对应的行从 txt 里删掉，其余内容一字不动：
// 空行、# 注释、本轮没成功的链接，以及原来的行尾符（CRLF 文件不会被改成 LF）。
//
// 匹配用与去重相同的 urlKey，所以清单里写的是 `.../abc-123/`、
// 而任务里记的是 `.../abc-123` 时也能对上，不会因为一个斜杠漏删。
func PruneURLs(path string, urls []string) (PruneResult, error) {
	res := PruneResult{Path: path}
	if strings.TrimSpace(path) == "" {
		return res, errors.New("未选择待下载 txt 文件")
	}

	drop := make(map[string]bool, len(urls))
	for _, u := range urls {
		drop[urlKey(u)] = true
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return res, fmt.Errorf("读取 txt 失败: %w", err)
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}

	// 按 "\n" 切而不是逐行 Scanner：切出来的片段自带 "\r"，
	// 原样拼回去就保住了 CRLF 文件的行尾。
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		key := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if i == 0 {
			// 首行的 UTF-8 BOM 去掉再匹配；写回时仍带着它，不改用户的文件编码。
			key = strings.TrimSpace(strings.TrimPrefix(key, "\ufeff"))
		}
		if key == "" || strings.HasPrefix(key, "#") {
			kept = append(kept, line)
			continue
		}
		if drop[urlKey(key)] {
			res.Removed++
			continue
		}
		res.Kept++
		kept = append(kept, line)
	}

	// 一条都没命中就不动文件：不必为了「什么都没变」去改一次修改时间。
	if res.Removed == 0 {
		return res, nil
	}
	if err := writeFileAtomic(path, []byte(strings.Join(kept, "\n")), mode); err != nil {
		return res, fmt.Errorf("写入 txt 失败: %w", err)
	}
	return res, nil
}

// writeFileAtomic 先写同目录临时文件再改名：中途失败（磁盘满、权限不足、
// 被别的程序占用）时原文件还在，不会留下写了一半的清单。
//
// 错误只描述失败环节，不假定用途：同一份实现也被完成记录（.jabgrab-done）用，
// 那边把错误包成「写完成记录失败」，这里包成「写入 txt 失败」。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".jabgrab-tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	name := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(name)
	}

	if _, err := tmp.Write(data); err != nil {
		discard()
		return fmt.Errorf("写入失败: %w", err)
	}
	// 保留原文件的权限位：临时文件默认是 0600，直接改名会把用户改过的
	// 0644/0664 悄悄变成只有自己可读。
	if err := tmp.Chmod(mode); err != nil {
		discard()
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("替换失败: %w", err)
	}
	return nil
}
