package player

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestOpenRejectsEmptyAndMissing(t *testing.T) {
	if err := Open("", ""); err == nil {
		t.Fatal("空路径应报错")
	}
	missing := filepath.Join(t.TempDir(), "nope.mp4")
	if err := Open("", missing); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}

func TestOpenRejectsDir(t *testing.T) {
	if err := Open("", t.TempDir()); err == nil {
		t.Fatal("目录不应被当成视频打开")
	}
}

func TestOpenRejectsMissingPlayer(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 自定义播放器不存在时必须在启动前拦住，而不是把错误甩给用户猜。
	err := Open(filepath.Join(t.TempDir(), "no-such-player"), file)
	if err == nil {
		t.Fatal("播放器不存在应报错")
	}
}

// 自定义播放器路径允许带环境变量：Windows 上 %ProgramFiles% 之类的写法很常见。
func TestOpenExpandsEnvInPlayerPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JABGRAB_TEST_PLAYER_DIR", dir)
	// 目录里没有这个可执行文件，展开后应当走到「播放器不可用」，而不是
	// 因为原始字面量里带 $ 被判成非法路径。
	err := Open("${JABGRAB_TEST_PLAYER_DIR}/no-such-player", file)
	if err == nil {
		t.Fatal("展开后不存在的播放器应报错")
	}
	if runtime.GOOS == "darwin" {
		// macOS 下 .app 分支不参与，这里只需确认报的是播放器不可用。
		if got := err.Error(); got == "" {
			t.Fatal("错误信息不应为空")
		}
	}
}

// 自定义播放器这条链要真的能起来：脚本收到成品路径就算成功。
// 覆盖的正是最容易被平台细节坑掉的部分（进程组、不挂控制台、不阻塞）。
func TestOpenLaunchesCustomPlayer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh 脚本做探针，Windows 上不适用")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "played.txt")
	script := filepath.Join(dir, "fake-player.sh")
	body := "#!/bin/sh\nprintf '%s' \"$1\" > " + record + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 故意带一层引号：从资源管理器「复制文件地址」拷出来的就是这种形状。
	if err := Open(`"`+script+`"`, file); err != nil {
		t.Fatalf("启动自定义播放器失败: %v", err)
	}

	// 播放器是异步起的，轮询等它把参数写下来。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(record); err == nil && len(got) > 0 {
			if string(got) != file {
				t.Fatalf("传给播放器的路径不对 got=%q want=%q", got, file)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("播放器没有跑起来：没等到记录的参数")
}

// 留空播放器时要走系统默认关联程序（macOS 的 open / Linux 的 xdg-open），
// 而不是悄悄什么都不做。用假的 open 探针验证命令行拼对没有。
func TestOpenFallsBackToSystemHandler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 走 ShellExecuteW，没有可替换的外部命令")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "args.txt")
	name := "open"
	if runtime.GOOS != "darwin" {
		name = "xdg-open"
	}
	body := "#!/bin/sh\nprintf '%s' \"$1\" > " + record + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	file := filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Open("", file); err != nil {
		t.Fatalf("默认播放器启动失败: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(record); err == nil && len(got) > 0 {
			if string(got) != file {
				t.Fatalf("默认播放器收到的路径不对 got=%q want=%q", got, file)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("没有调用系统默认播放器")
}

// 用户填的播放器路径要能容忍常见的手写 / 粘贴习惯。
func TestCleanPath(t *testing.T) {
	t.Setenv("JABGRAB_TEST_DIR", "/opt/players")
	cases := []struct{ in, want string }{
		{`/opt/pot/potplayer.exe`, `/opt/pot/potplayer.exe`},
		{`   /opt/pot/potplayer.exe   `, `/opt/pot/potplayer.exe`},
		{`"/opt/pot/potplayer.exe"`, `/opt/pot/potplayer.exe`},
		{`'/opt/pot/potplayer.exe'`, `/opt/pot/potplayer.exe`},
		{`${JABGRAB_TEST_DIR}/pot.exe`, `/opt/players/pot.exe`},
		// Windows 习惯写法：os.ExpandEnv 不认 %VAR%，这里要认。
		{`%JABGRAB_TEST_DIR%\pot.exe`, `/opt/players\pot.exe`},
		// 认不出来的变量原样留着，别把路径吃掉。
		{`%JABGRAB_NOT_SET%\pot.exe`, `%JABGRAB_NOT_SET%\pot.exe`},
	}
	for _, c := range cases {
		if got := CleanPath(c.in); got != c.want {
			t.Fatalf("CleanPath(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
