package component

import (
	"runtime"
	"strings"
	"testing"
)

const sampleManifest = `{
  "schema": 1,
  "components": {
    "scraper": {
      "version": "0.4.3",
      "protocol": 1,
      "assets": {
        "darwin/arm64":  { "url": "https://example.com/scraper-macos-arm64.zip", "size": 20500000, "sha256": "` + sampleSHA + `" },
        "windows/amd64": { "url": "https://example.com/scraper-windows-amd64.zip", "size": 21800000, "sha256": "` + sampleSHA + `" }
      }
    }
  }
}`

const sampleSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestManifestSpecSelectsPlatform 确认按 GOOS/GOARCH 选包。
//
// 键必须用 GOOS（darwin）而不是发布文件名里的 macos：多一张对照表就多一处
// 会漂移的地方。这里显式断言 darwin 能命中，等于把这条约定钉住。
func TestManifestSpecSelectsPlatform(t *testing.T) {
	m, err := ParseManifest([]byte(sampleManifest))
	if err != nil {
		t.Fatalf("解析清单: %v", err)
	}

	spec, err := m.Spec("scraper", "darwin", "arm64")
	if err != nil {
		t.Fatalf("darwin/arm64 应命中: %v", err)
	}
	if spec.Version != "0.4.3" || spec.Protocol != 1 {
		t.Fatalf("字段未带出：%+v", spec)
	}
	if spec.Asset.URL != "https://example.com/scraper-macos-arm64.zip" {
		t.Fatalf("选错了包：%s", spec.Asset.URL)
	}

	if _, err := m.Spec("scraper", "linux", "arm64"); err == nil {
		t.Fatal("清单里没有 linux/arm64 却成功了")
	}
	if _, err := m.Spec("ffprobe", "darwin", "arm64"); err == nil {
		t.Fatal("清单里没有 ffprobe 却成功了")
	}
}

// TestManifestSpecForCurrent 确认当前平台走通（本机跑测试时至少 darwin/arm64 有）。
func TestManifestSpecForCurrent(t *testing.T) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	if key != "darwin/arm64" && key != "windows/amd64" {
		t.Skipf("样本清单不含当前平台 %s", key)
	}
	m, err := ParseManifest([]byte(sampleManifest))
	if err != nil {
		t.Fatalf("解析清单: %v", err)
	}
	if _, err := m.SpecForCurrent("scraper"); err != nil {
		t.Fatalf("当前平台应命中: %v", err)
	}
}

// TestParseManifestRejectsBadInput 确认坏清单在解析期就被拒绝。
func TestParseManifestRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"空对象":        `{}`,
		"schema 不支持": `{"schema": 99, "components": {"scraper": {}}}`,
		"不是 JSON":    `not json`,
		"组件为空":       `{"schema": 1, "components": {}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(payload)); err == nil {
				t.Fatalf("%s 却解析成功", name)
			}
		})
	}
}

// TestManifestSpecValidatesAsset 确认清单里写坏的资产在取用时就被拒绝。
//
// 例如校验码少一位：不在这里拒绝，用户会先下完 20MB 再看到「校验失败」，
// 而且每次重试都一样，很容易被当成网络问题。
func TestManifestSpecValidatesAsset(t *testing.T) {
	payload := strings.Replace(sampleManifest, sampleSHA, strings.Repeat("a", 63), 1)
	m, err := ParseManifest([]byte(payload))
	if err != nil {
		t.Fatalf("解析清单: %v", err)
	}
	if _, err := m.Spec("scraper", "darwin", "arm64"); err == nil {
		t.Fatal("校验码位数不对却通过了")
	}
}

// TestAssetURLForMirror 确认镜像按前缀语义拼接。
func TestAssetURLForMirror(t *testing.T) {
	asset := Asset{URL: "https://github.com/o/r/releases/download/v1/a.zip"}

	if got := asset.URLFor(""); got != asset.URL {
		t.Fatalf("未设镜像时应原样返回：%s", got)
	}
	if got := asset.URLFor("  "); got != asset.URL {
		t.Fatalf("镜像为空白时应原样返回：%s", got)
	}
	want := "https://mirror.example.com/https://github.com/o/r/releases/download/v1/a.zip"
	if got := asset.URLFor("https://mirror.example.com/"); got != want {
		t.Fatalf("镜像拼接错误：%s", got)
	}
}
