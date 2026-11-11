# 构建 Windows 发行目录：Go/Wails 主程序 + Python 爬取程序 + N_m3u8DL-RE。
#
# 产物（dist 为扁平目录，三个可执行文件平铺，运行时按同级目录自动定位）：
#   dist\jabgrab.exe
#   dist\N_m3u8DL-RE.exe
#   dist\jab-scrape.exe  + _internal\
#
# 用法:
#   pwsh -File scripts\build.ps1                 # 全量
#   pwsh -File scripts\build.ps1 -SkipPython     # 只重建 Go 外壳
#   pwsh -File scripts\build.ps1 -SkipGo         # 只重打 Python 爬取程序
[CmdletBinding()]
param(
    [switch]$SkipPython,
    [switch]$SkipGo
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$GoDir = Join-Path $Root 'go'
$PyDir = Join-Path $Root 'python'
$Dist = Join-Path $Root 'dist'
$PyBuild = Join-Path $PyDir 'build\pyi'

function Step([string]$Text) {
    Write-Host ''
    Write-Host "==> $Text" -ForegroundColor Cyan
}

function Assert-Command([string]$Name, [string]$Hint) {
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "缺少 $Name。$Hint"
    }
}

# PowerShell 里原生命令的非零退出码不会打断脚本，统一包一层。
function Invoke-Checked([string]$Exe, [string[]]$Argv) {
    & $Exe @Argv
    if ($LASTEXITCODE -ne 0) {
        throw "$Exe $($Argv -join ' ') 失败（exit $LASTEXITCODE）"
    }
}

Assert-Command 'uv' '请先安装 uv：https://docs.astral.sh/uv/'
Assert-Command 'go' '请先安装 Go 工具链'

# PyInstaller 参数与 macOS / CI 共用一份。
$pyArgs = Get-Content (Join-Path $Root 'scripts\pyinstaller.args') |
    ForEach-Object { $_.Trim() } |
    Where-Object { $_ -ne '' -and -not $_.StartsWith('#') }

if (-not $SkipPython) {
    Step '1/4 打包 Python 爬取程序（PyInstaller onedir）'
    Push-Location $PyDir
    try {
        Invoke-Checked 'uv' @('sync')
        if (Test-Path $PyBuild) { Remove-Item $PyBuild -Recurse -Force }
        Invoke-Checked 'uv' (@('run', 'pyinstaller') + $pyArgs)
    }
    finally { Pop-Location }
    $size = (Get-ChildItem (Join-Path $PyBuild 'dist\jab-scrape') -Recurse -File |
            Measure-Object Length -Sum).Sum / 1MB
    Write-Host ('    {0:N1} MB → build\pyi\dist\jab-scrape' -f $size)
}

if (-not $SkipGo) {
    Step '2/4 构建 Go / Wails 应用'
    Push-Location $GoDir
    try {
        # 版本取自 go.mod，避免和依赖里的 wails 版本脱节
        $match = Select-String -Path 'go.mod' -Pattern 'wailsapp/wails/v2 v(\d+\.\d+\.\d+)'
        if (-not $match) { throw 'go.mod 里没找到 wails/v2 版本' }
        $wailsVer = $match.Matches[0].Groups[1].Value
        Invoke-Checked 'go' @(
            'run', "github.com/wailsapp/wails/v2/cmd/wails@v$wailsVer",
            'build', '-clean', '-platform', 'windows/amd64'
        )
    }
    finally { Pop-Location }
    Write-Host '    → go\build\bin\jabgrab.exe'
}

Step '3/4 组装发行目录'
$dlr = Join-Path $GoDir 'third_party\N_m3u8DL-RE.exe'
if (-not (Test-Path $dlr)) {
    throw "缺少 $dlr。它是 Windows 版下载器，可从 N_m3u8DL-RE 的 v0.6.0-beta 发布页取 win-x64 包解压得到。"
}
if (Test-Path $Dist) { Remove-Item $Dist -Recurse -Force }
New-Item -ItemType Directory -Path $Dist | Out-Null
Copy-Item (Join-Path $GoDir 'build\bin\jabgrab.exe') $Dist
Copy-Item $dlr $Dist
Copy-Item (Join-Path $PyBuild 'dist\jab-scrape\*') $Dist -Recurse -Force
Write-Host '    → dist\jabgrab.exe + N_m3u8DL-RE.exe + jab-scrape.exe'

Step '4/4 自检'
& (Join-Path $Dist 'jabgrab.exe') --selftest
if ($LASTEXITCODE -ne 0) {
    Write-Warning '自检未通过，检查 dist 里三件套是否齐全'
}

$total = [math]::Round(((Get-ChildItem $Dist -Recurse -File | Measure-Object Length -Sum).Sum / 1MB), 1)
Write-Host ''
Write-Host "完成，发行目录：$Dist（$total MB）" -ForegroundColor Green
