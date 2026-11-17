package component

import (
	_ "embed"
	"errors"
	"sync"
)

// builtinRaw 是编译进 App 的兜底清单。
//
// 为什么需要它
// ------------
// 在线清单（Release 上的 manifest.json）是首选：它随发布更新，不必发一版 App。
// 但用户可能断网、GitHub 不可达、或所在网络需要代理，那时至少还要能回答
// 「缺的是哪个版本、多大、能不能装」——否则界面上只能显示「未知」，而用户
// 连「装不上是因为没网」都判断不出来。
//
// 为什么是文件而不是 -ldflags 注入
// --------------------------------
// 注入 JSON 字符串要过两层引号（shell 一层、链接器 flag 一层），Windows 的
// PowerShell 与 bash 写法不同，出错时表现为「链接成功但变量是空的」，没有任何
// 报错。文件没有这个问题。CI 在每个平台的构建 job 里用
// `manifestgen -only <当前平台>` 生成它，因此内容与同一次构建的产物严格对应。
//
// 仓库里提交的是空清单（components 为空）：本地开发构建拿不到兜底，只能走
// 在线清单 —— 这是刻意的，提交一份真实版本号会在下一次发布后立刻变成假信息。
//
//go:embed manifest_builtin.json
var builtinRaw []byte

var (
	builtinOnce sync.Once
	builtin     Manifest
	builtinErr  error
)

// BuiltinManifest 返回编译进 App 的兜底清单，没有可用清单时返回错误。
func BuiltinManifest() (Manifest, error) {
	builtinOnce.Do(func() {
		if len(builtinRaw) == 0 {
			builtinErr = errors.New("未内置组件清单")
			return
		}
		builtin, builtinErr = ParseManifest(builtinRaw)
	})
	if builtinErr != nil {
		return Manifest{}, builtinErr
	}
	return builtin, nil
}
