//go:build windows

package downloader

import (
	"fmt"
	"unicode/utf16"

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
