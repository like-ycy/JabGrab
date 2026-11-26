# 生成组件包（scraper / dlre）并写入 dist\release\（Windows 侧）。
#
# 与 tools/component.sh 同责：打包与命名交给 cmd\manifestgen -pack，
# 它会跑一遍产物取自述版本、按自述版本命名，并按 App 侧的解析路径校验结构。
#
# 用法：tools\component.ps1 -Version 0.8.3
[CmdletBinding()]
param(
    [string]$Version = ''
)

$ErrorActionPreference = 'Stop'

$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$Config = @{}
Get-Content (Join-Path $PSScriptRoot 'config.env') | ForEach-Object {
    $line = $_.Trim()
    if ($line -eq '' -or $line.StartsWith('#')) { return }
    $i = $line.IndexOf('=')
    if ($i -lt 1) { return }
    $key = $line.Substring(0, $i).Trim()
    $value = $line.Substring($i + 1).Trim()
    # config.env 同时喂给 bash，多值只能写成 COMPONENTS="scraper dlre"（不写引号
    # 会被 source 拆成命令）。PowerShell 拿到的是带引号的字面量，不剥掉就会拼出
    # third_party\"scraper"\windows-amd64 这种路径。
    if ($value.Length -ge 2 -and
        (($value.StartsWith('"') -and $value.EndsWith('"')) -or
         ($value.StartsWith("'") -and $value.EndsWith("'")))) {
        $value = $value.Substring(1, $value.Length - 2)
    }
    $Config[$key] = $value
}
$HasPython = $Config['HAS_PYTHON']
$PyBuildName = $Config['PY_BUILD_NAME']

function Die([string]$Text) { throw $Text }
function Step([string]$Text) { Write-Host ''; Write-Host "==> $Text" -ForegroundColor Cyan }

if ($HasPython -ne '1') { Die '模板 A 没有组件包' }

$rel = Join-Path $Root 'dist\release'
New-Item -ItemType Directory -Path $rel -Force | Out-Null

$stage = Join-Path ([IO.Path]::GetTempPath()) ('jabgrab-pkg-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path (Join-Path $stage 'scraper') -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stage 'dlre') -Force | Out-Null

$pyOut = Join-Path $Root "python\build\pyi\dist\$PyBuildName"
if (-not (Test-Path $pyOut)) { Die "缺少 Python 产物：$pyOut" }
Copy-Item (Join-Path $pyOut '*') (Join-Path $stage 'scraper') -Recurse -Force

$dlre = Join-Path $Root 'third_party\dlre\windows-amd64\N_m3u8DL-RE.exe'
if (-not (Test-Path $dlre)) { Die "缺少 $dlre（可用 tools\fetch-dlre.sh win-x64 获取）" }
Copy-Item $dlre (Join-Path $stage 'dlre\N_m3u8DL-RE.exe')

Step '生成组件包（windows/amd64）'
Push-Location (Join-Path $Root 'go')
try {
    & go run ./cmd/manifestgen -pack -src (Join-Path $stage 'scraper') -name scraper `
        -binary jab-scrape -protocol 1 -slug windows -arch amd64 -out-dir $rel
    if ($LASTEXITCODE -ne 0) { Die '打包 scraper 失败' }
    & go run ./cmd/manifestgen -pack -src (Join-Path $stage 'dlre') -name dlre `
        -binary N_m3u8DL-RE -protocol 1 -slug windows -arch amd64 -out-dir $rel
    if ($LASTEXITCODE -ne 0) { Die '打包 dlre 失败' }
}
finally { Pop-Location }

Remove-Item -Recurse -Force $stage
Get-ChildItem $rel | Select-Object -ExpandProperty Name
