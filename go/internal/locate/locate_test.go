package locate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"jabgrab/internal/config"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"uv run jab-scrape", []string{"uv", "run", "jab-scrape"}},
		{`"/Applications/My App/bin/jab" --x`, []string{"/Applications/My App/bin/jab", "--x"}},
		{"'/a b/c' d", []string{"/a b/c", "d"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := splitCommand(c.in); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("splitCommand(%q)=%v want=%v", c.in, got, c.want)
		}
	}
}

func TestDlrUsesConfigPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "N_m3u8DL-RE")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.DlrPath = bin

	got, src, err := Dlr(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != bin || src != "配置指定" {
		t.Fatalf("应使用配置路径 got=%q src=%q", got, src)
	}
}

func TestDlrRejectsBadConfigPath(t *testing.T) {
	cfg := config.Default()
	cfg.DlrPath = filepath.Join(t.TempDir(), "missing")
	if _, _, err := Dlr(cfg); err == nil {
		t.Fatal("配置路径不存在时应报错，而不是静默回退")
	}
}

func TestScrapeFromCustomCommand(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "jab-scrape")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.PythonCmd = bin

	argv, workDir, err := Scrape(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 1 || argv[0] != bin {
		t.Fatalf("参数不符: %v", argv)
	}
	if workDir != dir {
		t.Fatalf("工作目录应为 %q，实际 %q", dir, workDir)
	}
}

func TestScrapeCommandWithArgs(t *testing.T) {
	cfg := config.Default()
	cfg.PythonCmd = "uv run jab-scrape"

	argv, _, err := Scrape(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"uv", "run", "jab-scrape"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("参数不符 got=%v want=%v", argv, want)
	}
}
