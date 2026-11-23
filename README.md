# JabGrab

把一份视频页面 URL 列表，变成保存目录里命名规整、按演员归档的 mp4。

JabGrab 是一个 macOS / Windows 桌面应用：Wails（Go + 原生 JS 前端）做外壳与任务编排，
Python 侧车 `jab-scrape` 负责用浏览器抓取 m3u8 地址，实际下载交给
[N_m3u8DL-RE](https://github.com/nilaoda/N_m3u8DL-RE)。

## 功能

- **批量**：读一个 txt（一行一个 URL），自动去重，只处理已启用站点 + 关键字命中的链接。
- **顺序执行**：爬一个、下一个，并发固定为 1，不重试、不落库；单条失败不影响后续。
- **实时进度**：任务列表逐行显示状态、百分比、速度、ETA、已下载 / 总大小，另有运行日志面板。
- **按演员归档**：保存到 `保存目录/演员名/`，多个演员用横线连接；取不到演员就放在保存目录根。
- **跳过已存在**：按文件名与大小判断，已下载过的直接标 `已存在`，不重复花流量。
- **清单自清理**：点「清除已下载」把本轮成功的 URL（`完成` 与 `已存在`）从 txt 里删掉，
  失败与未处理的留在原处——下次点「开始」就只重试这几条。注释、空行、行尾符原样保留。
  界面上的任务队列同时清空，不留上一轮的状态。
- **中文字幕版优先**：页面带中文字幕时文件名追加 `-C`，这一份落地后再清理同名的普通版旧档。
- **一键播放 / 定位**：下载成功的每一行右侧出现「播放」与「打开文件夹」两个按钮：前者用系统
  默认播放器（也可指定 PotPlayer 等）打开这一条，后者在访达 / 资源管理器里定位到它。
  不会自动播，什么时候看由你决定，想自己整理文件也一步就能跳过去。
- **组件按需下载**：默认轻量包只有外壳（约 4.6 MB），缺爬取程序 / 下载器时在设置页一键装。
- **可诊断**：`--selftest` 不开窗口，直接打印三件套（爬取程序 / 下载器 / ffmpeg）各自从哪来。

## 工作流程

```
txt (URL 列表)
   │  ReadURLs：去空行 / 去重 / 按站点关键字分类，未命中的忽略
   ▼
┌────────────────────────── 逐条处理（并发 1） ──────────────────────────┐
│ 1. jab-scrape <url> --source jable|missav                           │
│      → stdout 一行 JSON：m3u8_url / video_name / headers / actors    │
│ 2. 判断是否已存在（文件名 + 大小）→ 是则跳过                          │
│ 3. N_m3u8DL-RE --save-dir <演员目录> --save-name <标题> ...          │
│      → 逐行解析进度帧，推送到前端                                    │
│ 4. 中文字幕版落地后删除同名普通版                                    │
└─────────────────────────────────────────────────────────────────────┘
```

每一步的定位（用哪个爬取命令、哪个下载器、哪个 ffmpeg）都由 `internal/locate` 按固定顺序解析：
**配置指定 → 已安装组件 → 随包 `tools/` → 程序同级目录 → 开发目录 → 系统 PATH**。
设置页会把实际命中的路径与来源（`配置指定` / `组件` / `随包` / `程序同级目录` / `开发目录` / `系统 PATH`）
直接显示出来，出问题时不用猜。

## 快速开始

### 1. 下载

在 [Releases](https://github.com/like-ycy/JabGrab/releases) 里选一种：

| 包 | 体积 | 适用 |
|---|---|---|
| `jabgrab_<ver>_<os>_<arch>.zip` | ≈ 4.6 MB | 默认选它，联网使用，缺的组件首次运行时下载 |
| `jabgrab_<ver>_full_<os>_<arch>.zip` | ≈ 30 MB | 离线 / 内网，爬取程序与下载器已随包 |

macOS 取 `darwin_arm64`（Apple Silicon）或 `darwin_amd64`（Intel），Windows 取 `windows_amd64`。

### 2. 装 ffmpeg（必需）

`N_m3u8DL-RE` **启动时就会校验 ffmpeg**，缺了它连下载都不会开始：

```bash
# macOS
brew install ffmpeg
# Windows：装完后把 ffmpeg.exe 所在目录加进 PATH，或直接放到下载器同目录
```

### 3. 首次运行

1. macOS 解压后把 `JabGrab.app` 拖进「应用程序」；Windows 解压到任意目录后运行 `jabgrab.exe`。
2. 打开 **设置** 页，在「外部组件」里下载 `爬取程序` 与 `下载器`（整包已自带，可跳过）。
3. 选好 **视频保存目录**（默认 `~/Downloads/JabGrab`）和 **待下载 txt**，保存设置。
4. 回 **任务** 页点「开始」。顶部状态灯会显示本次运行的环境自检结论。

> macOS 发布包未做 Apple 公证。若提示「无法打开」，右键 →「打开」，或
> `xattr -dr com.apple.quarantine /Applications/JabGrab.app`。

## 三个外部依赖

| 依赖 | 谁来提供 | 缺失时的症状 |
|---|---|---|
| `jab-scrape` | 本项目用 PyInstaller 打包 | 提示「未找到爬取程序」 |
| `N_m3u8DL-RE` | 上游 Release，版本由 `scripts/fetch-dlre.sh` 钉死 | 提示「未找到 N_m3u8DL-RE」 |
| `ffmpeg` | 用户自装 | `N_m3u8DL-RE` 启动即拒绝运行 |

组件装在 `<配置目录>/components/`（见下节），与 App 本体分离：换爬取规则不必重发
30 MB 的包，升级 App 也不必重下几十 MB 的运行时。设置页可填**组件下载源**前缀走镜像
（如 `https://ghproxy.example/`），清单请求与包下载都会经过它；出于校验码可信的考虑，
除本机回环外必须是 https。

## 配置

配置文件：

- macOS `~/Library/Application Support/jabgrab/config.json`
- Windows `%APPDATA%\jabgrab\config.json`

设置环境变量 `JABGRAB_CONFIG_DIR` 可整体改到别处（便携 / 测试用）。设置页能改全部常用项，
也可以直接编辑这个文件；**文件存在但语法错误时程序只用默认值运行，不会覆盖你的原文件**。

| 字段 | 默认 | 说明 |
|---|---|---|
| `saveDir` | `~/Downloads/JabGrab` | 视频保存根目录 |
| `txtPath` | — | 待下载 URL 列表 |
| `recentTxt` | `[]` | 最近用过的 txt，最多 8 条 |
| `dlrPath` | 空 | 留空走定位链 |
| `pythonCmd` | 空 | 留空则自动探测，可填 `uv run --project /path/to/python jab-scrape` |
| `ffmpegPath` | 空 | 留空走定位链（含下载器同目录） |
| `playerPath` | 空 | 点行内「播放」时用哪个播放器：留空交给系统默认，可填 PotPlayer 等可执行文件，成品路径作为唯一参数传入 |
| `componentsOnDemand` | `true` | 只是「缺组件时是否显示下载入口」，已装的组件永远生效 |
| `componentMirror` | 空 | 组件下载源前缀，空则直连 GitHub |
| `sites` | jable / missav 均启用 | 每项 `{enabled, keyword}`，URL 含 keyword（不区分大小写）即归属该站点；命中多个取关键字最长的 |
| `download.threads` | 25 | `--thread-count` |
| `download.retryCount` | 20 | `--download-retry-count` |
| `download.timeoutSec` | 30 | `--http-request-timeout` |
| `download.maxSpeed` | 空 | `--max-speed`，如 `15M`，空为不限速 |
| `download.muxToMP4` | `true` | 下完混流成 mp4 |
| `scrapeTimeoutSec` | 180 | 单条爬取超时（开浏览器比下载慢，单独设） |

## 从源码构建

### 环境

- Go ≥ 1.27（见 `go/go.mod`）
- Node 不需要：前端是手写 HTML/CSS/JS，构建时直接 `embed` 进二进制
- [uv](https://docs.astral.sh/uv/) + Python ≥ 3.10（`python/.python-version` 已固定）
- macOS：Xcode Command Line Tools；Windows：WebView2 运行时
- Wails CLI 不必全局安装，脚本与下文都用 `go run .../cmd/wails@<版本>`，版本取自 `go/go.mod`

### 开发

```bash
cd python && uv sync          # 首次：装爬虫依赖
cd go    && go run github.com/wailsapp/wails/v2/cmd/wails@v2.17.1 dev
```

开发模式下 `internal/locate` 会向上找到含 `python/pyproject.toml` 的目录，直接用
`uv run --project <python> jab-scrape`，不需要先打包侧车。下载器用
`go/third_party/` 里随仓库内置的那一份。

### 打包发行目录

```bash
scripts/build.sh                  # 轻量包：dist/ 只有 App
scripts/build.sh --full           # 整包：额外带 tools/scraper/ 与 tools/dlre/
scripts/build.sh --legacy-flat    # 旧扁平布局，仅用于验证老用户升级路径
scripts/build.sh --no-py --no-go  # 复用已有产物，只重新组装目录
```

Windows 用 `pwsh -File scripts\build.ps1 [-Full] [-LegacyFlat] [-SkipPython] [-SkipGo]`。
相关脚本：

| 脚本 | 作用 |
|---|---|
| `scripts/build.sh` / `build.ps1` | 组装发行目录（1/5～5/5 五步，含自检） |
| `scripts/build-py.sh` | PyInstaller onedir 打包 `jab-scrape`，参数取自 `scripts/pyinstaller.args` |
| `scripts/fetch-dlre.sh` | 拉取指定平台的 `N_m3u8DL-RE`（仓库内置 arm64 与 win-x64，Intel Mac 用它补 x64） |
| `go run ./cmd/manifestgen` | 生成组件清单 `manifest.json` 与产物自述，复用 App 侧同一套 struct 校验 |

### 测试与自检

```bash
cd go     && go test ./...        # Go：组件安装、配置、下载器解析、定位链、流水线
cd python && uv run pytest        # Python：站点解析器（不打网络）
cd python && uv run ruff check    # 静态检查
```

产物级自检（验证「每个组件从哪来」，CI 与本地都跑）：

```bash
./dist/JabGrab.app/Contents/MacOS/jabgrab --selftest   # macOS
dist\jabgrab.exe --selftest                            # Windows
jab-scrape --selftest                                  # 校验侧车里模块链是否齐全
```

## 项目结构

```
go/
  main.go            Wails 入口，--selftest 短路
  app.go             绑定给前端的方法：Boot / Start / Stop / SaveConfig ...
  components.go      组件服务的导出层（列表 / 安装 / 卸载 / 进度）
  selftest.go        --selftest 实现
  internal/
    config/          配置读写、默认值补全、站点匹配
    locate/          三件套定位链（配置 → 组件 → 随包 → 同级 → 开发 → PATH）
    scraper/         调 Python 侧车，解析单行 JSON
    downloader/      拼 N_m3u8DL-RE 参数，解析进度帧
    pipeline/        主流程：读 txt → 去重分类 → 逐条爬取 → 下载
    player/          打开成品：系统默认程序（ShellExecuteW / open）或自定义播放器
    reveal/          打开成品所在目录：访达（open -R）/ 资源管理器（ShellExecuteW /select,）
    component/       组件清单、下载、校验、安装、文件锁
    proc/            跨平台子进程（Windows 无黑窗 / UTF-8 输出）
  frontend/dist/     原生 JS 界面，构建时 embed
  third_party/       内置 N_m3u8DL-RE（macOS arm64 / Windows x64）
  cmd/manifestgen/   清单生成工具
python/
  src/jab_scrape/    CLI 入口、jable / missav 解析器、结果模型
  tests/             解析器单测
scripts/             构建、打包、拉取上游二进制
docs/                发布形态拆分的设计与实测记录
.github/workflows/   Release 流水线
```

## 发布

推送形如 `v1.2.3` 的 tag 触发 `.github/workflows/release.yml`，产出 **14 个资产**：
3 个轻量包、3 个整包、3 个 `scraper` 组件包、3 个 `dlre` 组件包，加 `manifest.json`
与 `SHA256SUMS.txt`。组件包必须齐 `windows/amd64`、`darwin/arm64`、`darwin/amd64` 三个平台，
否则 manifest 校验阶段直接失败。

为什么拆、拆完省了多少、以及哪些地方不能照抄别的项目，见
[`docs/release-split-plan.md`](docs/release-split-plan.md)（含全部实测体积数据）。

## 免责声明

本工具只做「按 URL 抓 m3u8 并下载」这一件事，不含任何站点内容，也不内置任何搜索能力。
请只用于你有权访问与保存的内容，并自行遵守所在地区的法律与目标站点的条款；
使用本工具产生的后果由使用者承担。
