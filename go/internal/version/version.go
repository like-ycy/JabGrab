// Package version 提供应用版本，发布构建由 tools/build.* 按标签注入。
package version

// Version 未注入时明确显示为开发构建，不使用下载器组件的版本。
var Version = "0.0.0-dev"
