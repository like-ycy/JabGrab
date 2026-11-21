package player

import (
	"os"
	"regexp"
	"strings"
)

// winVarRE 匹配 Windows 习惯的 %VAR% 写法。
//
// 变量名允许括号，因为 Windows 上确实有 `ProgramFiles(x86)` 这种名字。
var winVarRE = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_.()]*)%`)

// ExpandPath 展开路径里的环境变量，两种写法都认：
//
//	$VAR / ${VAR}  —— 交给 os.ExpandEnv
//	%VAR%          —— Windows 的习惯写法，os.ExpandEnv 不认，这里补上
//
// 认不出来的变量原样保留：路径里偶尔出现 % 是合法的，不该被悄悄吃掉。
func ExpandPath(s string) string {
	s = os.ExpandEnv(s)
	if !strings.Contains(s, "%") {
		return s
	}
	return winVarRE.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

// CleanPath 把用户填的路径变成可直接使用的形式：去掉首尾空白与包裹的引号，
// 再展开环境变量。
//
// 引号那一步不是洁癖：从「复制文件地址」或快捷方式属性里拷出来的路径常常带一层
// 引号，直接拿去 os.Stat 一定失败，而用户看到的现象只是「明明填对了却说找不到」。
func CleanPath(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	// 展开后可能又冒出引号（环境变量本身存的就是 "C:\path" 这种），再收一次。
	return strings.Trim(strings.TrimSpace(ExpandPath(s)), `"'`)
}
