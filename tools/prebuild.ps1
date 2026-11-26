# 预构建钩子（Windows 侧）：把本平台的组件清单烘进 Go 代码，供断网时使用。
#
# 与 tools/prebuild.sh 同责。只在发布构建里生成：清单里的下载地址形如
# releases/download/<tag>，而 Release 只对 tag 存在，本地生成出来就是死链。
[CmdletBinding()]
param(
    [string]$Version = ''
)

$ErrorActionPreference = 'Stop'

$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
function Die([string]$Text) { throw $Text }
function Note([string]$Text) { Write-Host "    $Text" }

$tag = if ($env:RELEASE_TAG) { $env:RELEASE_TAG } else { (git -C $Root describe --tags --exact-match 2>$null) }
if (-not $tag) {
    Note '非 tag 构建：保持仓库内的空清单，组件安装走在线清单'
    exit 0
}

$repo = $env:GITHUB_REPOSITORY
if (-not $repo) {
    $url = (git -C $Root remote get-url origin 2>$null)
    if ($url) {
        $url = $url -replace '\.git$', ''
        $url = $url -replace '^[a-z+]+://', ''
        $url = $url -replace '^git@', ''
        $url = $url -replace ':', '/'
        $parts = $url -split '/'
        $repo = "$($parts[$parts.Count - 2])/$($parts[$parts.Count - 1])"
    }
}
if (-not $repo) { Die '无法确定仓库名，请设置 GITHUB_REPOSITORY' }

$base = "https://github.com/$repo/releases/download/$tag"
$out = Join-Path $Root 'go\internal\component\manifest_builtin.json'

Note "清单：$base（仅 windows/amd64）"
Push-Location (Join-Path $Root 'go')
try {
    & go run ./cmd/manifestgen -dir (Join-Path $Root 'dist\release') -base-url $base `
        -name scraper -binary jab-scrape -merge -only windows/amd64 -out $out
    if ($LASTEXITCODE -ne 0) { Die '生成 scraper 清单失败' }
    & go run ./cmd/manifestgen -dir (Join-Path $Root 'dist\release') -base-url $base `
        -name dlre -binary N_m3u8DL-RE -merge -only windows/amd64 -out $out
    if ($LASTEXITCODE -ne 0) { Die '生成 dlre 清单失败' }
}
finally { Pop-Location }

$size = (Get-Item $out).Length
if ($size -lt 100) { Die "内置清单异常（$size 字节）" }
Note "已写入 go\internal\component\manifest_builtin.json（$size 字节）"
