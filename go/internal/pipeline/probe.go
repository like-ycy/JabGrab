package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"jabgrab/internal/proc"
)

// durationRe 抓 ffmpeg 读容器头时打印的 Duration 行。
var durationRe = regexp.MustCompile(`Duration:\s*(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)

// mediaDurationFn 是 mediaDuration 的间接引用，测试里可替换：
// 判断「要不要删旧档」的用例不该依赖本机是否装了 ffmpeg。
var mediaDurationFn = mediaDuration

// mediaDuration 只读容器头拿时长，不解码整部片子。
//
// 用 ffmpeg 而不是 ffprobe：ffmpeg 已经是启动期硬依赖（缺了连下载都不开始），
// ffprobe 不一定随包。`ffmpeg -i <file>` 不指定输出时必然以退出码 1 结束，
// 这是正常现象，所以判定只看能不能从输出里解析出 Duration，不看退出码。
//
// 读不到就返回错误，调用方据此保留旧文件 —— 清理只是顺手收拾，
// 宁可多留一份，也不能凭猜删掉用户的正片。
func mediaDuration(ffmpeg, file string) (time.Duration, error) {
	if strings.TrimSpace(ffmpeg) == "" {
		return 0, errors.New("没有可用的 ffmpeg")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // 路径来自本应用的保存目录，不是用户拼接的命令行
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-i", file)
	// 与下载器/爬取器一致：Windows 上不弹黑框，unix 上进独立进程组。
	proc.Prepare(cmd)
	out, _ := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return 0, fmt.Errorf("读取 %s 的时长超时", file)
	}

	m := durationRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("%s 的容器头里没有可用的时长", file)
	}
	h, err1 := strconv.Atoi(string(m[1]))
	min, err2 := strconv.Atoi(string(m[2]))
	sec, err3 := strconv.ParseFloat(string(m[3]), 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, fmt.Errorf("%s 的时长字段无法解析", file)
	}
	d := time.Duration(h)*time.Hour + time.Duration(min)*time.Minute + time.Duration(sec*float64(time.Second))
	if d <= 0 {
		return 0, fmt.Errorf("%s 的时长为零", file)
	}
	return d, nil
}
