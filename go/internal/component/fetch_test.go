package component

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("缺少 Accept 头，某些镜像会返回 HTML 引导页")
		}
		_, _ = w.Write([]byte(`{"schema":1,"components":{"scraper":{"version":"0.4.3","protocol":1,"assets":{"darwin/arm64":{"url":"https://e.com/a.zip","size":10,"sha256":"` + strings.Repeat("a", 64) + `"}}}}}`))
	}))
	defer srv.Close()

	manifest, err := FetchManifest(context.Background(), srv.Client(), srv.URL+"/manifest.json", "")
	if err != nil {
		t.Fatalf("拉取清单: %v", err)
	}
	spec, err := manifest.Spec("scraper", "darwin", "arm64")
	if err != nil {
		t.Fatalf("取安装描述: %v", err)
	}
	if spec.Version != "0.4.3" || spec.Asset.Size != 10 {
		t.Fatalf("解析结果不对：%+v", spec)
	}
}

// TestFetchManifestErrors 覆盖三种「看得懂」的失败：状态码、非 JSON、超大响应。
func TestFetchManifestErrors(t *testing.T) {
	t.Run("状态码", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		if _, err := FetchManifest(context.Background(), srv.Client(), srv.URL, ""); err == nil ||
			!strings.Contains(err.Error(), "404") {
			t.Fatalf("期望报 404，实际 %v", err)
		}
	})

	t.Run("非 JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html>rate limited</html>"))
		}))
		defer srv.Close()
		if _, err := FetchManifest(context.Background(), srv.Client(), srv.URL, ""); err == nil {
			t.Fatal("HTML 不该被当成清单接受")
		}
	})

	t.Run("超大响应", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 2MB，超过上限。真实场景是被篡改的镜像返回垃圾。
			_, _ = w.Write([]byte(strings.Repeat("x", maxManifestBytes*2)))
		}))
		defer srv.Close()
		_, err := FetchManifest(context.Background(), srv.Client(), srv.URL, "")
		if err == nil || !strings.Contains(err.Error(), "过大") {
			t.Fatalf("期望报清单过大，实际 %v", err)
		}
	})
}

// TestFetchManifestAppliesMirror 确认清单请求也走镜像。
//
// 只给包加速而不给清单加速时，用户点下载的第一步（取清单）就失败了，
// 表现为「下载安装」按钮灰着。
func TestFetchManifestAppliesMirror(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"schema":1,"components":{"scraper":{"version":"1","protocol":1,"assets":{}}}}`))
	}))
	defer srv.Close()

	_, _ = FetchManifest(context.Background(), srv.Client(), "https://github.com/o/r/releases/download/v1/manifest.json", srv.URL)
	if gotPath != "/https://github.com/o/r/releases/download/v1/manifest.json" {
		t.Fatalf("镜像未按前缀拼接，实际路径 = %q", gotPath)
	}
}

// TestBuiltinManifest 覆盖内置清单的两种合法状态。
//
// 仓库里提交的是空占位（提交真实清单会在下次发布后变成假信息），CI 在构建
// 前用 `manifestgen -only <本平台>` 覆盖它。两种状态下这个函数都必须是
// 「要么明确没有，要么给出的条目可用」，不能返回半个清单。
func TestBuiltinManifest(t *testing.T) {
	manifest, err := BuiltinManifest()
	if err != nil {
		// 占位清单：ParseManifest 因为 components 为空而失败，这是预期形态。
		if !strings.Contains(err.Error(), "为空") {
			t.Fatalf("空占位清单应报「清单为空」，实际 %v", err)
		}
		return
	}
	if len(manifest.Components) == 0 {
		t.Fatal("解析成功却没有任何组件")
	}
	for name := range manifest.Components {
		if _, err := manifest.SpecForCurrent(name); err != nil {
			t.Fatalf("内置清单里的 %s 在当前平台不可用: %v", name, err)
		}
	}
}

// TestValidateURLRequiresHTTPS 确认明文地址被拒、回环放行。
//
// 纯 http 下，能改写响应的一方可以同时替换包与清单里的校验码 ——
// 「校验通过」于是什么也没证明。回环地址放行是因为测试与本地联调走 httptest，
// 它不经过网络。
func TestValidateURLRequiresHTTPS(t *testing.T) {
	ok := []string{
		"https://github.com/o/r/releases/download/v1/a.zip",
		"http://127.0.0.1:8080/a.zip",
		"http://localhost:8080/a.zip",
		"http://[::1]:8080/a.zip",
	}
	for _, raw := range ok {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("%s 应当允许：%v", raw, err)
		}
	}

	bad := []string{
		"http://example.com/a.zip",
		"ftp://example.com/a.zip",
		"https://",
	}
	for _, raw := range bad {
		if err := ValidateURL(raw); err == nil {
			t.Errorf("%s 应当被拒绝", raw)
		}
	}
}

// TestHTTPClientRejectsInsecureRedirect 确认重定向到明文地址会被拦下。
//
// 只验证初始地址时，一个 https 的清单源可以 302 到 http://… ，默认客户端
// 照跟 —— 于是「必须 https」这条限制形同虚设。
func TestHTTPClientRejectsInsecureRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/manifest.json", http.StatusFound)
	}))
	defer srv.Close()

	// 用回环的 http 起步（ValidateURL 放行），重定向目标不是回环。
	_, err := FetchManifest(context.Background(), NewHTTPClient(5*time.Second), srv.URL+"/manifest.json", "")
	if err == nil || !strings.Contains(err.Error(), "不安全") {
		t.Fatalf("期望拦下不安全重定向，实际 %v", err)
	}
}

// TestHTTPClientAllowsLoopbackRedirect 确认回环内部的重定向仍然可用（本地联调）。
func TestHTTPClientAllowsLoopbackRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema":1,"components":{"x":{"version":"1","protocol":0,"assets":{"darwin/arm64":{"url":"https://e.com/a.zip","size":1,"sha256":"` + strings.Repeat("a", 64) + `"}}}}}`))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/manifest.json", http.StatusFound)
	}))
	defer srv.Close()

	manifest, err := FetchManifest(context.Background(), NewHTTPClient(5*time.Second), srv.URL+"/manifest.json", "")
	if err != nil {
		t.Fatalf("回环重定向应当允许: %v", err)
	}
	if _, ok := manifest.Components["x"]; !ok {
		t.Fatal("清单没解析出来")
	}
}
