package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// 不开窗口，只报告运行时定位结果（用于验证发行目录）
	if len(os.Args) > 1 && (os.Args[1] == "--selftest" || os.Args[1] == "-selftest") {
		os.Exit(selfTest())
	}

	app := NewApp()

	err := wails.Run(&options.App{
		Title:            "JabGrab",
		Width:            1000,
		Height:           720,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: &options.RGBA{R: 246, G: 247, B: 250, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []any{app},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarDefault(),
			About: &mac.AboutInfo{
				Title:   "JabGrab",
				Message: "m3u8 抓取与下载",
			},
		},
	})
	if err != nil {
		println("启动失败:", err.Error())
	}
}
