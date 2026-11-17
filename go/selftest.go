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
//
// 它是发布流程里唯一能在「用户机器形态」上跑的检查：CI 打包完就调用它，
// 用来验证三件套（爬取程序 / 下载器 / ffmpeg）在该布局下都能被找到。
// 四种形态（轻量包未装组件、轻量包装了组件、整包、旧扁平包）都要能说清
// 「每个组件从哪来」，见 docs/release-split-plan.md §13。
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

	// 组件服务只用来查「装没装」，不联网：断网时自检也要能给出结论。
	svc := newComponentService()
	candidates := func(name string) []locate.Candidate {
		if p := svc.path(name); p != "" {
			return []locate.Candidate{{Path: p, Source: locate.SourceComponent}}
		}
		return nil
	}

	if argv, dir, src, err := locate.ScrapeSource(cfg, candidates(locate.NameScraper)...); err != nil {
		fmt.Println("爬取程序: 未找到 —", err)
		code = 1
	} else {
		fmt.Println("爬取程序:", strings.Join(argv, " "))
		fmt.Println("          来源:", src)
		if dir != "" {
			fmt.Println("          工作目录:", dir)
		}
	}

	dlrPath := ""
	if p, src, err := locate.Dlr(cfg, candidates(locate.NameDlr)...); err != nil {
		fmt.Println("下载器:   未找到 —", err)
		code = 1
	} else {
		dlrPath = p
		fmt.Println("下载器:  ", p)
		fmt.Println("          来源:", src)
	}

	var near []string
	if dlrPath != "" {
		near = append(near, dirOf(dlrPath))
	}
	if p, src := locate.FFmpeg(cfg, near, candidates(locate.NameFFmpeg)...); p == "" {
		fmt.Println("ffmpeg:   未找到 — N_m3u8DL-RE 启动时会拒绝运行")
		code = 1
	} else {
		fmt.Println("ffmpeg:  ", p)
		fmt.Println("          来源:", src)
	}

	// 组件区块：装没装、什么版本、能不能更新。这些都不联网。
	if svc.available() {
		fmt.Println("\n组件目录:", svc.rootDir())
		for _, def := range componentDefs() {
			state := svc.manager.LocalState(def.Name, def.Protocol).String()
			version, _, installed := svc.manager.Installed(def.Name)
			line := fmt.Sprintf("  %-8s %s", def.Name, state)
			if installed {
				line += "（已装 " + version + "）"
			}
			if spec, _, err := svc.specLocal(def); err == nil {
				line += " 可装 " + spec.Version
			}
			fmt.Println(line)
		}
	} else {
		fmt.Println("\n组件目录: 不可用 —", svc.err)
	}

	if issues := cfg.Check(); len(issues) > 0 {
		fmt.Println("\n待处理:")
		for _, s := range issues {
			fmt.Println(" ·", s)
		}
	}
	return code
}
