package main

import (
	"fmt"
	"os"
	"strings"

	"jabgrab/internal/config"
	"jabgrab/internal/locate"
)

// selfTest 打印运行时定位结果并以退出码反映可用性。
// 用法：jabgrab --selftest
// 用于验证打包后的发行目录里三个可执行文件是否都能被找到。
func selfTest() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("配置: 读取失败 (%v)，已用默认值\n", err)
	}

	code := 0
	fmt.Println("== JabGrab 自检 ==")

	if p, err := config.Path(); err == nil {
		fmt.Println("配置文件:", p)
		if _, statErr := os.Stat(p); statErr == nil {
			fmt.Println("          已存在")
		} else {
			fmt.Println("          尚未创建（首次启动会生成）")
		}
	}

	if exe, err := os.Executable(); err == nil {
		fmt.Println("主程序:  ", exe)
	}
	fmt.Println("保存目录:", cfg.SaveDir)

	if p, src, err := locate.Dlr(cfg); err != nil {
		fmt.Println("下载器:   未找到 —", err)
		code = 1
	} else {
		fmt.Println("下载器:  ", p)
		fmt.Println("          来源:", src)
	}

	if argv, dir, err := locate.Scrape(cfg); err != nil {
		fmt.Println("爬取:     未找到 —", err)
		code = 1
	} else {
		fmt.Println("爬取:    ", strings.Join(argv, " "))
		if dir != "" {
			fmt.Println("          工作目录:", dir)
		}
	}

	if issues := cfg.Check(); len(issues) > 0 {
		fmt.Println("\n待处理:")
		for _, s := range issues {
			fmt.Println(" ·", s)
		}
	}
	return code
}
