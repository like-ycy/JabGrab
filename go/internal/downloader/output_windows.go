//go:build windows

package downloader

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

// decodeOutput 与 proc.Prepare 的 CREATE_NO_WINDOW 配套：无控制台时，
// dl-re 的 .NET Console.OutputEncoding 使用系统 ANSI 代码页（中文 Windows 通常为 936）。
// 必须先保留完整行的原始字节再转码，不能先 ReadRune，否则中文会被替换且无法恢复。
func decodeOutput(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	codePage := windows.GetACP()
	wide := make([]uint16, len(raw))
	n, err := windows.MultiByteToWideChar(codePage, 0, &raw[0], int32(len(raw)), &wide[0], int32(len(wide)))
	if err != nil {
		return "", fmt.Errorf("解码 Windows 代码页 %d: %w", codePage, err)
	}
	return string(utf16.Decode(wide[:n])), nil
}

// safeCut 把切点回退到当前代码页的字符边界。
//
// 不能靠「前一个字节是否在 0x81-0xFE」判断首字节：双字节代码页（936/950 等）的
// 尾字节同样可能落在 0x81-0xFE，那样会把一个完整字符切成两半。这里直接拿解码
// 结果验：以替换字符结尾说明切在了字符中间，退一个字节再看，最多试几个字节。
// 单个字符最多 4 字节，所以循环有界；找不到干净边界时退回 limit，只影响边界上
// 一两个字符的显示。
func safeCut(buf []byte, limit int) int {
	if limit > len(buf) {
		limit = len(buf)
	}
	for n := limit; n > 0 && n > limit-utf8.UTFMax; n-- {
		s, err := decodeOutput(buf[:n])
		if err == nil && !strings.HasSuffix(s, "\uFFFD") {
			return n
		}
	}
	return limit
}
