//go:build !windows

package downloader

import "unicode/utf8"

func decodeOutput(raw []byte) (string, error) {
	return string(raw), nil
}

// safeCut 把切点回退到 UTF-8 字符边界。
//
// pending 的末尾可能是被读取边界截断的半个字符（一次读正好停在多字节字符中间），
// 所以「已经是缓冲区末尾」并不等于「可以放行」：半个字符会被解码成 U+FFFD，
// 另一半在下一段里再坏一次，日志就此永久损坏。末尾字符不完整时退到它的起点，
// 整段留给下一次读取。
func safeCut(buf []byte, limit int) int {
	if limit > len(buf) {
		limit = len(buf)
	}
	for k := limit; k > 0; k-- {
		if k == len(buf) {
			// DecodeLastRune 对被截断的末尾字符返回 (RuneError, 1)。
			if r, size := utf8.DecodeLastRune(buf); r == utf8.RuneError && size == 1 {
				continue
			}
			return k
		}
		if utf8.RuneStart(buf[k]) { // buf[k] 起头即边界，buf[:k] 停在字符之间
			return k
		}
	}
	return limit
}
