# 构建 Windows 发行目录：Go/Wails 主程序 + （整包时）爬取程序与下载器。
#
# 两种形态（默认轻量）：
#   pwsh -File scripts\build.ps1          轻量包：只有 jabgrab.exe，组件按需下载
#   pwsh -File scripts\build.ps1 -Full    整包：带 tools\scraper\ 与 tools\dlre\
#   pwsh -File scripts\build.ps1 -LegacyFlat
#                                         旧扁平布局（三件套平铺在 dist 根），
#                                         只用于排查「旧包用户升级后还能不能用」
#
# 目录契约与 App 侧 go/internal/locate 共同维护，详见 docs/release-split-plan.md §4。
#
# 用法:
#   pwsh -File scripts\build.ps1 -SkipPython     # 只重建 Go 外壳
#   pwsh -File scripts\build.ps1 -SkipGo         # 只重打 Python 爬取程序
[CmdletBinding()]
param(
    [switch]$SkipPython,
    [switch]$SkipGo,
    [switch]$Full,
    [switch]$LegacyFlat
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$GoDir = Join-Path $Root 'go'
$PyDir = Join-Path $Root 'python'
$Dist = Join-Path $Root 'dist'
$PyBuild = Join-Path $PyDir 'build\pyi'
$PyOut = Join-Path $PyBuild 'dist\jab-scrape'

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

# 剔除产物里两类确定不该发出去的东西。CI 走的是 scripts/build-py.sh（bash），
# 这里是本地 Windows 构建的等价实现 —— 两边改动要同步，否则会出现「CI 的包 35MB、
# 本地的包 70MB」这种只在发布后才发现的差异。
function Remove-UnwantedPayload([string]$Out) {
    $before = (Get-ChildItem $Out -Recurse -File | Measure-Object Length -Sum).Sum / 1MB

    # 1) selenium 自带的三份 Selenium Manager（四平台共 16.9MB），永不被调用：
    #    SeleniumBase 始终显式把驱动路径传给 ChromeService。
    $managers = Get-ChildItem $Out -Recurse -Filter 'selenium-manager*' -File
    foreach ($f in $managers) { Remove-Item $f.FullName -Force }
    $stray = Get-ChildItem $Out -Recurse -Filter 'selenium-manager*' -File
    if ($stray) { throw "产物里仍有 selenium-manager：$($stray.FullName -join ', ')" }

    # 2) 驱动目录只留 __init__.py：SeleniumBase 在**运行时**把 chromedriver /
    #    uc_driver 下进 site-packages/seleniumbase/drivers/，--collect-all 会把它
    #    原样收进产物。驱动必须与用户机器的 Chrome 匹配，不能由发布者代劳。
    $drivers = Join-Path $Out '_internal\seleniumbase\drivers'
    if (Test-Path $drivers) {
        Get-ChildItem $drivers -Recurse -File |
            Where-Object { $_.Name -ne '__init__.py' -and $_.Extension -ne '.pyc' } |
            ForEach-Object { Remove-Item $_.FullName -Force }
        $left = Get-ChildItem $drivers -Recurse -File |
            Where-Object { $_.Name -ne '__init__.py' -and $_.Extension -ne '.pyc' }
        if ($left) { throw "产物里仍带着驱动文件：$($left.FullName -join ', ')" }
    }

    $after = (Get-ChildItem $Out -Recurse -File | Measure-Object Length -Sum).Sum / 1MB
    Write-Host ('    {0:N1} MB（剔除前 {1:N1} MB）→ build\pyi\dist\jab-scrape' -f $after, $before)
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
    Remove-UnwantedPayload $PyOut
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
if (($Full -or $LegacyFlat) -and -not (Test-Path $dlr)) {
    throw "缺少 $dlr。它是 Windows 版下载器，可从 N_m3u8DL-RE 的 v0.6.0-beta 发布页取 win-x64 包解压得到。"
}
if (Test-Path $Dist) { Remove-Item $Dist -Recurse -Force }
New-Item -ItemType Directory -Path $Dist | Out-Null
Copy-Item (Join-Path $GoDir 'build\bin\jabgrab.exe') $Dist

if ($Full -or $LegacyFlat) {
    $scraperDir = Join-Path $Dist 'tools\scraper'
    $dlreDir = Join-Path $Dist 'tools\dlre'
    New-Item -ItemType Directory -Path $scraperDir, $dlreDir -Force | Out-Null
    Copy-Item (Join-Path $PyOut '*') $scraperDir -Recurse -Force
    Copy-Item $dlr $dlreDir
    Write-Host '    → dist\jabgrab.exe + tools\scraper + tools\dlre'

    if ($LegacyFlat) {
        # 旧扁平布局：与 v0.3.0 的 zip 一致，验证老用户升级后仍可用。
        Remove-Item (Join-Path $Dist 'tools') -Recurse -Force
        Copy-Item $dlr $Dist
        Copy-Item (Join-Path $PyOut '*') $Dist -Recurse -Force
        Write-Host '    → 旧扁平布局：dist\jabgrab.exe + N_m3u8DL-RE.exe + jab-scrape.exe'
    }
}
else {
    Write-Host '    轻量包：不含 tools\，首次使用时按需下载爬取程序与下载器'
}

Step '4/4 自检'
# 必须 Start-Process -Wait：wails 生产构建给 Windows 产物加了 -H windowsgui，
# 是 GUI 子系统程序，用 & 调用时 PowerShell 不会等待退出，$LASTEXITCODE 保持
# $null，会被误判成失败。
$selftest = Start-Process -FilePath (Join-Path $Dist 'jabgrab.exe') `
    -ArgumentList '--selftest' -NoNewWindow -Wait -PassThru
if ($selftest.ExitCode -ne 0) {
    if ($Full -or $LegacyFlat) {
        throw "整包自检失败（exit $($selftest.ExitCode)）：三件套没找齐，检查 tools\ 布局"
    }
    Write-Warning "自检退出码 $($selftest.ExitCode)。轻量包缺组件是预期结果，装完组件后应为 0"
}

$total = [math]::Round(((Get-ChildItem $Dist -Recurse -File | Measure-Object Length -Sum).Sum / 1MB), 1)
Write-Host ''
Write-Host "完成，发行目录：$Dist（$total MB）" -ForegroundColor Green
