# 发布形态拆分：设计与落地记录

> 目标：把「半年不动」的东西从每次发版里拿出去，同时不让用户多操一步。
> 参照 `../video_library` 的 `2b4e7bc`（发布默认改轻量包、CI 拆两类产物），
> 但它没遇到的三处问题必须我们自己解决，见 §3。
>
> 状态：P0–P3 已落地（代码、脚本、CI、界面），P4（ffmpeg 组件）只做机制。
> 所有体积数字均为本机 macOS arm64 实测（2026-11-17）。

## 0. 结果

| 项 | 改前 | 改后 |
|---|---|---|
| 默认下载（轻量包） | 39 MB | **4.6 MB** |
| 整包（离线用） | 39 MB | 30 MB |
| 改一次 Go 的发版成本 | 39 MB | **4.6 MB** |
| Python 侧车 | 50 MB / 30 MB(zip) | 35 MB / 20 MB(zip) |
| Release 资产数 | 3 | 14 |

拆分**不减少**完整安装的总下载（4.6 + 20 + 5.9 ≈ 30 MB，与整包持平），
它减少的是「每发一次版用户要重下多少」与「改 App 要不要连带上游二进制」。
App 只占整包的 15%，剩下 85% 是几乎不动的 CPython 运行时和上游下载器。

## 1. 现状（改前实测）

### 1.1 体积构成

| 组成 | 目录体积 | 压缩后 | 变更频率 |
|---|---|---|---|
| `JabGrab.app`（Wails，Go + 前端） | 8.3 MB | 3.5 MB | 每次发版 |
| `python/`（PyInstaller onedir 侧车） | 50 MB | 30 MB | 半年一次 |
| `N_m3u8DL-RE`（上游二进制） | 12 MB | 5.2 MB | 跟上游版本，几乎不动 |
| **合计** | **71 MB** | **39 MB** | — |

`python/` 的 50 MB 里，自己的代码只有 348 行，其余是：

| 项 | 体积 |
|---|---|
| `_internal/libpython3.14.dylib` | 17 MB |
| `_internal/selenium/webdriver/common/*/selenium-manager*` | **16.9 MB** |
| `jab-scrape`（exe，含 PYZ） | 12 MB |
| `_internal/mycdp` + `seleniumbase` + 其余 | ≈1.6 MB |

那份 16.9 MB 是**四个平台各一份**的 Selenium Manager（macos 3.4 / linux-arm64 4.1 /
linux-x86_64 5.2 / windows 3.5 + 660KB 元数据），打包时被 `--collect-all seleniumbase`
全带进来，而它们一次都不会被调用 —— 理由见 §10。

### 1.2 改前的布局（这就是必须改的地方）

macOS：`JabGrab.app/` + `N_m3u8DL-RE` + `python/jab-scrape`（三件套平铺）
Windows：`jabgrab.exe` + `N_m3u8DL-RE.exe` + `jab-scrape.exe` + `_internal/`（全平铺）

### 1.3 三个外部依赖

| 依赖 | 来源 | 缺失时的用户可见症状 |
|---|---|---|
| `jab-scrape` | 我们用 PyInstaller 构建 | 点开始即报「未找到爬取程序」 |
| `N_m3u8DL-RE` | 上游 GitHub Release，版本由 `tools/fetch-dlre.sh` 钉死 | 「未找到 N_m3u8DL-RE」 |
| `ffmpeg` | 用户自装（brew / PATH / 放下载器同目录） | `N_m3u8DL-RE` **启动即校验**，直接拒绝运行 |

## 2. 产物形态

### 2.1 资产矩阵（14 个）

```
Release v0.4.0
├── jabgrab_0.4.0_darwin_arm64.zip            4.6 MB  ← 轻量包，默认下载
├── jabgrab_0.4.0_darwin_amd64.zip
├── jabgrab_0.4.0_windows_amd64.zip
├── jabgrab_0.4.0_full_darwin_arm64.zip       30 MB   ← 整包，离线/内网
├── jabgrab_0.4.0_full_darwin_amd64.zip
├── jabgrab_0.4.0_full_windows_amd64.zip
├── scraper_0.1.0_darwin_arm64.zip            20 MB   ← 组件，App 按需下载
├── scraper_0.1.0_darwin_amd64.zip
├── scraper_0.1.0_windows_amd64.zip
├── dlre_0.6.0_darwin_arm64.zip               5.9 MB  ← 组件
├── dlre_0.6.0_darwin_amd64.zip
├── dlre_0.6.0_windows_amd64.zip
├── manifest.json                             < 1 KB  ← 组件清单
└── SHA256SUMS.txt
```

### 2.2 为什么不是「全包 + 增量包」

如果「增量包」指**二进制 diff 补丁**，这条路不走：

- 补丁只能针对一个**确定的旧版本**生成。用户手里的版本分布是黑箱（有人在用
  v0.2.0、有人手动改过文件），要么为 N 个旧版本各出一份补丁，要么假设大家都
  是上一个版本 —— 一旦不是，补丁静默失败，兜底还是整包。
- 我们的「冷资产」不是 App 的一部分，而是**独立上游**（CPython 运行时、
  N_m3u8DL-RE）。它们该有自己的版本号与发布节奏，而不是被打成 App 的补丁。

所以采用「按组件独立发版」：组件只在自己变了的时候发新版，App 每次只带 4.6 MB。

### 2.3 为什么 Go 不单独成包

Wails 产物就是 App 本体，Go 编译进去不可拆。Go 改得频繁是事实，但它的产物只有
4.6 MB —— 每次都重发是合理的，不值得再为它发明一层。

## 3. 五处不能照搬 video_library 的地方

| # | 差异 | video_library 的做法 | grabber 的做法 |
|---|---|---|---|
| 1 | **布局是扁平的** | 一开始就是 `tools/scraper/` | 现在三件套平铺。Windows 上「两个 PyInstaller onedir 侧车」会把两份 `_internal/` 撞在一起 —— 必须引入子目录，且旧扁平布局要继续能跑（老用户手里的 zip） |
| 2 | **组件是三个，不是一个** | 只有 scraper | scraper / dlre / ffmpeg 三个，各有自己的版本源与缺失后果 |
| 3 | **dlre 的版本自述带 `+commit`** | 版本号干净 | `N_m3u8DL-RE --version` 输出 `0.6.0+df70f0b3...`，`+` 不能进文件名。拆成 `version=0.6.0` + `revision=df70f0b` |
| 4 | **scraper 没有 `--version`** | `scraper version` 是既有子命令 | 原本只有 `--help`。清单版本必须来自产物自述，所以先加 `--version`（现在还有 `--selftest`） |
| 5 | **协议是两个** | 单一 NDJSON | scraper 是「argv + 单行 JSON」，dlre 是「argv + **进度文本行**」。后者才脆：`downloader.go` 靠解析输出算百分比，上游改一版文案，进度条就坏了 —— App 不崩，只是「一直 0%」。协议版本对 dlre 比 scraper 更重要 |

补充：**ffmpeg 是启动期硬依赖**，不是可选增强（`app.go` 与 `pipeline.go` 都提前拦）。
它的机制与 scraper 相同，只是暂不发布资产（§11 P4）。

## 4. 目录布局契约

### 4.1 随包布局（整包）

```
macOS:   JabGrab.app/   tools/scraper/{jab-scrape,_internal/}   tools/dlre/N_m3u8DL-RE
Windows: jabgrab.exe    tools/scraper/{jab-scrape.exe,_internal/}   tools/dlre/N_m3u8DL-RE.exe
```

`tools/<name>/` 由 `tools/build.sh`、`tools/build.ps1` 定义，App 侧
`go/internal/locate` 按同一路径探测。**不带版本号**：随包的东西天然只有一个版本。

轻量包只有 `JabGrab.app` / `jabgrab.exe`，没有 `tools/`（CI 有反向断言）。

### 4.2 已安装组件（另一套目录）

```
macOS   ~/Library/Application Support/jabgrab/components/scraper-0.1.0/
Windows %APPDATA%\jabgrab\components\scraper-0.1.0\
```

**带版本号**，与随包相反：这里要并存「正在跑的旧版」与「刚下的新版」（Windows
上正在执行的 exe 不能被覆盖），旧版本留到下次启动再清。

### 4.2.1 zip 的顶层结构

上面是 `dist/` 的布局，也就是**解压之后**的布局。整包 zip 在这之外多包一层
`JabGrab/`，解压出来只有一个目录，不会把 App 与 `tools/` 散在下载目录里：

```
jabgrab_<ver>_full_<os>_<arch>.zip
└── JabGrab/
    ├── JabGrab.app（macOS）/ jabgrab.exe（Windows）
    └── tools/scraper、tools/dlre
```

里面的相对位置与 4.1 完全一致，所以 `locate` 不用改。轻量包不套这层：它只有一个
文件，套层只会让用户多点开一次；同时轻量包里**连空的 `tools/` 目录都不许有**
（zip 会把空目录也收进去，用户解压后看到空文件夹会以为组件没下全），CI 两步都有断言。

### 4.3 旧扁平布局继续可用

`locate` 的搜索链里保留「exe 同级 / 同级 bin/ / 同级 python/」三档，所以 v0.3.0
的 zip 解压后照样能用。`tools/build.sh --legacy-flat` 可以造一份出来验证。

## 5. manifest schema

```json
{
  "schema": 1,
  "components": {
    "scraper": {
      "version": "0.1.0",
      "protocol": 1,
      "binary": "jab-scrape",
      "assets": {
        "darwin/arm64": { "url": "…/scraper_0.1.0_darwin_arm64.zip", "size": 20478815, "sha256": "…" }
      }
    },
    "dlre": {
      "version": "0.6.0",
      "revision": "df70f0b3da0c630bd413bf617e758051f6b64757",
      "protocol": 1,
      "binary": "N_m3u8DL-RE",
      "assets": { "darwin/arm64": { "url": "…", "size": 5925141, "sha256": "…" } }
    }
  }
}
```

与 video_library 同一套字段名（`schema` / `components.<name>.{version,protocol,binary,assets}` /
`assets.<goos>/<goarch>.{url,size,sha256}`），所以 `internal/component` 的解析与
安装代码可以整段移植。

差异只有一处：`revision`。dlre 的版本自述带 commit hash，`version` 取 `+` 之前，
hash 放 `revision`，用于跨平台一致性断言。

### 文件名约定

| 类型 | 模式 | 例 |
|---|---|---|
| 轻量包 | `jabgrab_<appver>_<os>_<arch>.zip` | `jabgrab_0.4.0_darwin_arm64.zip` |
| 整包 | `jabgrab_<appver>_full_<os>_<arch>.zip` | `jabgrab_0.4.0_full_windows_amd64.zip` |
| 组件包 | `<name>_<ver>_<os>_<arch>.zip` | `scraper_0.1.0_windows_amd64.zip` |

文件名里的 `os` 是 `darwin` / `windows`（与 `jabgrab_*` 一致），manifest 的 key 是
Go 的 `GOOS/GOARCH`。这层映射只存在于 `manifestgen` 一处（`slugToGOOS`），不在
CI 的 shell 里再拼一次。

## 6. 路径解析优先级

三种工具走同一条链，**顺序本身就是契约**（`go/internal/locate`）：

```
1. 配置显式路径         DlrPath / PythonCmd / FFmpegPath（用户意向最高，错了报错不回退）
2. 已安装组件           <config>/components/<name>-<ver>/<bin>
3. 随包 tools/<name>/   <Dist>/tools/<name>/<bin>
4. 旧扁平布局           exe 同级 / 同级 bin/ / 同级 python/
5. 开发布局             third_party/、cwd 向上找 python/pyproject.toml → uv run
6. 系统 PATH
```

- **2 在 3 之前**：用户装过新版组件时，不该被包内的旧副本压过。整包用户没装过
  组件，3 直接命中，行为不变。
- **3 在 4 之前**：`tools/` 是构建产出的确定来源，比「手动散放」可信；手动放置的
  用户仍有第 1 档。
- **来源要一起返回**（`locate.ScrapeSource` / `Dlr` / `FFmpeg` 都返回 source）。
  「装了组件却不生效」这类问题，第一眼要看来源 —— 六档里有两档可能指向同名的
  不同文件。设置页与 `--selftest` 都显示它。

注入候选的接口是 `locate.Resolver`（`func(name string) []locate.Candidate`），
由 `main` 包实现：`locate` 只判断「路径能不能跑」，不反向依赖组件管理。

## 7. 协议版本

| 组件 | 协议是什么 | 当前值 | 什么情况 bump |
|---|---|---|---|
| `scraper` | argv `<url> --source <jable\|missav>` + stdout 单行 JSON（`ok`/`m3u8_url`/`video_name`/`headers`/`actors`/`error`）+ 退出码 0/1 | 1 | 删字段、改语义、改参数名、改退出码含义。**只增字段不 bump** |
| `dlre` | ① argv 参数集；② **stdout 进度行格式**（`downloader.go` 靠它算百分比） | 1 | 上游改参数名、改进度文本。最容易被忽略：App 不崩，只是「一直 0%」 |
| `ffmpeg` | 无契约，能跑 `-version` 即可 | 0 | — |

装组件之前先比协议：不匹配就拒绝安装并提示升级 App，而不是装完在运行时才发现。
协议不匹配时退回内置清单（同一次构建写进 App 的，协议必然匹配）。

## 8. 代码落点（已落地）

### 8.1 Go

| 文件 | 内容 |
|---|---|
| `go/internal/component/`（新增） | 从 video_library 移植：`Manager`/`Spec`/`State`/`Install`/`Remove`/`CleanupStale`/`Verify`、跨进程文件锁（`lock_unix.go`/`lock_windows.go`）、磁盘空间检查、zip 解压（含符号链接越界防护）。三处必改：`DefaultDir()` 走 `config.Dir()`；`builtin.go` 改为 `//go:embed manifest_builtin.json`；User-Agent 与注释里的产品名 |
| `go/internal/component/manifest_builtin.json`（新增） | 提交空壳 `{"schema":1,"components":{}}`；tag 构建时 CI 覆盖成「本平台两份组件」。**不能 gitignore**：它是 `//go:embed` 的编译前置 |
| `go/cmd/manifestgen/`（新增） | 三种模式：生成清单（`-name … -merge`）、`-pack`（打包组件 + 写自述，按产物自述的版本命名）、`-meta`（只写自述）。dlre 的 `+commit` 在这里切开 |
| `go/components.go`（新增） | `componentService`（清单缓存 TTL 6h、协议校验、镜像前缀）、`ComponentDTO`、`ListComponents`/`InstallComponent`/`RemoveComponent`、进度事件（100ms 限流） |
| `go/internal/locate/locate.go` | `Candidate`/`Resolver`、六档来源常量、`tools/` 布局、`FFmpeg`（新增）、`ScrapeSource`（返回来源） |
| `go/internal/config/config.go` | `ComponentsOnDemand *bool`（nil=未设置→默认开）、`ComponentMirror`、`FFmpegPath` |
| `go/internal/pipeline/pipeline.go` | 三件套定位改走注入链；`Hooks.Resolve`；ffmpeg 路径随 `downloader.Options` 透传（原来每次 `FindFFmpeg` 重算） |
| `go/app.go` | 组件服务初始化与启动清理、`BootInfo` 增加组件状态/来源/组件目录、`Start` 的 ffmpeg 预检走定位链 |
| `go/selftest.go` | 三个工具的路径 + 来源、组件目录与两个组件的状态，退出码反映「三件套齐不齐」 |

### 8.2 Python

| 文件 | 内容 |
|---|---|
| `python/src/jab_scrape/_version.py`（新增） | 版本号唯一来源（默认 0.1.0），CI 不再注入 tag |
| `python/src/jab_scrape/__main__.py` | `--version`（只打一行）、`--selftest`（逐个 import 站点解析器与 seleniumbase）；站点解析器改为惰性导入 |
| `python/src/jab_scrape/__init__.py` | 不再 eager import `scraper`（会拉起 seleniumbase），改用 `__getattr__` 按需导入 |

### 8.3 前端（`go/frontend/dist/`，原生 JS）

| 文件 | 内容 |
|---|---|
| `index.html` | 设置页「外部组件」卡片区（组件卡、下载源、开关、安装目录）；任务页缺口提示条；路径区补 ffmpeg 输入框 |
| `app.js` | `COMPONENT_STATE` 映射、`renderComponents`、`renderComponentBanner`、安装/卸载调用、`component:*` 事件订阅、配置新字段 |
| `style.css` | `.comp` / `.comp-path` / `.comp-actions` / `button.link` |

### 8.4 脚本

| 文件 | 内容 |
|---|---|
| `python/packaging/pyinstaller.spec` | `--optimize 2` 等价物；`--hidden-import jab_scrape.jable` / `.missav`（惰性导入后 PyInstaller 看不到它们） |
| `python/packaging/pyinstaller.spec` | 在 Analysis 阶段剔除 selenium-manager 与运行时驱动（不再打包后删文件） |
| `tools/build.sh` | 三种形态（默认轻量 / `--full` / `--legacy-flat`）+ `--no-py`/`--no-go`（CI 只重新组装） |
| `tools/build.ps1` | 同上（PowerShell 等价实现，含 `-Full` / `-LegacyFlat` / `-SkipPython` / `-SkipGo`） |
| `tools/fetch-dlre.sh` | 角色扩成「三平台通用取件」，CI 用它拉上游二进制并校验 sha256 |

## 9. CI（`.github/workflows/release.yml`）

保持三个 job（`macos` 矩阵 / `windows` / `release`），不新增 `build-scraper`：
PyInstaller 必须在原生 runner 上跑，独立 job 只能把三平台再各启一遍机器，白等
一次 `uv sync` 与几分钟打包。

每个 build job 的步骤序列：

```
checkout → Go + uv（带缓存）
  → fetch-dlre.sh 取本平台下载器（不用仓库内置那份：CI 现拉才与钉住的 sha 一致）
  → 写入 App 版本（wails.json；爬取程序的版本不读 tag，见下）
  → build-py.sh（含剔除与断言）→ jab-scrape --selftest
  → 组装组件目录 out/{scraper,dlre} → manifestgen -pack（打包 + 自述 + 命名）
  → manifestgen 自校验本平台两份组件
  → （仅 tag）manifestgen -only 覆盖 manifest_builtin.json
  → wails build（//go:embed 在这一步吃进清单）
  → build.sh --no-py --no-go（轻量）→ 断言无 tools/ → zip
  → build.sh --no-py --no-go --full（整包）→ 断言结构 + --selftest → zip
  → 上传 artifact
```

`release` job：收集 3+3+3+3 与 6 份自述 → 两次 `manifestgen -merge` 合成
`manifest.json` → 删自述 → `SHA256SUMS.txt` → 断言资产数 14 → `gh release`。

几点不做妥协：

- **组件包由 Go 打**（`manifestgen -pack`），不用 `zip -y` / `Compress-Archive`。
  包名必须与自述版本逐字一致，而版本只能从产物读 —— 让两个平台的 shell 各自
  完成「取版本 → 切 `+` → 拼名字 → 打包」是两份必然分叉的实现。
- **轻量与整包分开匹配**（`! -name '*_full_*'`）：文件名只差一个 `_full_`，同一个
  `find` 模式会把整包算进轻量包，数量对得上而「默认下载项」变成 30 MB。
- **反向断言**：轻量包断言不含 `tools/`，防止默认值被人改回整包后悄悄回涨 26 MB。
- **兜底清单只在 tag 构建生成**：清单地址形如 `releases/download/<ref>`，Release
  只对 tag 存在；分支构建写进去就是一批必然 404 的死链，界面显示「可下载」、
  点下去必然失败。

## 10. 瘦身：两个真实原因，各约 17 MB

### 10.1 剔除 selenium-manager（−16.9 MB）

实测产物里这六个文件全在，且一次都不会被调用：

```
_internal/selenium/webdriver/common/macos/selenium-manager          3.4 MB
_internal/selenium/webdriver/common/linux-arm64/selenium-manager    4.1 MB
_internal/selenium/webdriver/common/linux-x86_64/selenium-manager   5.2 MB
_internal/selenium/webdriver/common/windows/selenium-manager.exe    3.5 MB
_internal/selenium/webdriver/common/selenium-manager.cdx.json       372 KB
_internal/selenium/webdriver/common/selenium-manager-THIRD-PARTY-NOTICES.txt  288 KB
```

**为什么不会被调用**：SeleniumBase 始终自己解析驱动，
`browser_launcher.py` 里 `ChromeService(executable_path=LOCAL_CHROMEDRIVER)` 显式给了
路径，selenium 那条「没给路径 → 问 Selenium Manager」的分支永不进入。grabber 的
`scraper.py` 只写了 `SB(uc=True, test=True, ...)`，看起来「没配驱动」，但驱动解析
由 SeleniumBase 接管，结论相同。

实现：`python/packaging/pyinstaller.spec` 在 Analysis 阶段过滤 `a.binaries` 与 `a.datas`，产物落盘前就不含这些文件。
一个 pattern 覆盖全部六个文件。不改 PyInstaller 参数是因为 CLI 没有「排除某些
data 文件」的选项，换 `.spec` 会引入平台分支，而 args 的「一份参数三平台共用」
是刻意设计。

### 10.2 清空运行时驱动目录（−37 MB，并且修掉一个**构建不确定性**）

这是落地过程中实测发现的、原方案没预料到的问题：

**SeleniumBase 在运行时把 `chromedriver` / `uc_driver` 下载进
`site-packages/seleniumbase/drivers/`**，而 `--collect-all seleniumbase` 会把这个目录
原样收进产物。于是同一份源码：

- 在干净的 CI 上打出 **35 MB**；
- 在「跑过一次真实爬取」的开发机上打出 **70 MB**（多出 chromedriver 17.7 MB +
  uc_driver 19.3 MB，mtime 就是那次爬取的时间）。

版本号相同、内容不同，而后者把**开发者本机的 Chrome 驱动版本**发给了所有用户。
驱动必须与用户机器上的 Chrome 匹配，所以 `build-py.sh` 把该目录清到只剩
`__init__.py`（含子目录里的），恢复「首次刮削时由 SeleniumBase 自己下」的形态。

### 10.3 `--optimize 2`（exe 12 MB → 10.2 MB）

去掉 docstring 与 assert。核实过：`seleniumbase` 4.57.1 源码 `^\s*assert ` **0 条**，
`core/`、`fixtures/` 不读 `__doc__`（grep 只命中 .pyc）。PyInstaller 6.22 支持
`--optimize LEVEL`，直接写进 args。**线上出问题时它是第一个该回退的开关**：删掉两行即可。

### 10.4 惰性导入的连带代价：`--hidden-import`

为了让 `--version` 能在「seleniumbase 装坏了」的现场也给出答案，`__main__.py` 不再
在顶层 `from .jable import …`。代价是 **PyInstaller 的静态分析看不到这两个模块**：

产物里没有 `jab_scrape.jable` / `jab_scrape.missav`，而 `--help` 与 `--version` 一切
正常，只有真正爬取时才 `ModuleNotFoundError`。修法是 args 里显式
`--hidden-import jab_scrape.jable` / `.missav`，并且加 `--selftest` 让 CI 把这类
问题拦在打包阶段（`--help` 根本覆盖不到）。

### 10.5 明确不做的三项

| 手段 | 结论 | 理由 |
|---|---|---|
| `strip` libpython dylib | 不做 | CPython 本身已 stripped，且会破坏 ad-hoc 签名 |
| UPX | 不做 | 杀软误判风险，启动无收益 |
| 收窄 `--collect-all seleniumbase` | 不做 | 动态导入密集，白名单收益 < 1 MB，换来的风险不值 |

### 10.6 实测数据

| 项 | 改前 | 改后 |
|---|---|---|
| `scraper` 目录 | 50 MB | **35 MB** |
| `jab-scrape` exe | 12 MB | 10.2 MB |
| `_internal/selenium/` | 17 MB | 0 |
| zip | 30 MB | **20 MB** |
| 构建耗时 | 20 s | 21 s |

## 11. 落地顺序与状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| **P0** | `--optimize 2` + 剔除 selenium-manager / 运行时驱动 + CI 断言 | 已完成 |
| **P1** | `jab-scrape --version`/`--selftest`；`tools/` 布局；`locate` 注入候选；旧布局兼容 | 已完成 |
| **P2** | `internal/component` + `manifestgen` + 内置兜底清单；CI 出组件包与清单 | 已完成 |
| **P3** | 默认轻量包 + 设置页组件卡片 + 任务页缺口提示 | 已完成 |
| **P4** | ffmpeg 组件（机制已就绪：`locate.FFmpeg` + `cfg.FFmpegPath`，未发布资产） | 机制完成 |

P4 未发布资产的理由：macOS 静态 ffmpeg 60–80 MB/平台，比 scraper 组件还大；而且
今天的用户大多已经装了（brew / PATH）。真要启用：CI 里把 ffmpeg 打成
`ffmpeg_<ver>_<os>_<arch>.zip`（`manifestgen -pack -name ffmpeg -binary ffmpeg
-version-args "-version" -protocol 0`），再在 `componentDefs()` 里加一行 ——
App 侧其余代码不用改。

P2 与 P3 原本建议分两次发版（先让整包与按需下载并存跑一个版本，再改默认值）。
一次做完的情况下，**回退手段是「整包仍然发布」**：把 Release 的默认下载项换成
`_full_` 即可，不需要改代码。

## 12. 风险清单

| 风险 | 影响 | 对策 |
|---|---|---|
| `--optimize 2` 导致真实爬取出异常 | 高 | 已核实无 assert/`__doc__` 依赖；出问题先回退 args 里那两行 |
| 剔除 selenium-manager 后 UC 模式拿不到驱动 | 高 | 驱动本就由 SeleniumBase 运行时下载；另有 `sbase get chromedriver` 兜底 |
| 发布包混入构建机的驱动版本 | 高（用户 Chrome 版本不匹配） | `build-py.sh` 清空驱动目录并断言 |
| GitHub 下载不可达，组件装不上 | 高（功能不可用） | 下载源可配镜像；整包仍发布；旧扁平布局继续可用 |
| 上游改进度输出 → 进度条恒 0% | 中 | `dlre` 的 `protocol` 在装之前拦 |
| 组件目录与随包副本混用导致版本漂移 | 中 | `installed.json` 单指针；设置页显示实际路径与来源 |
| 默认值被改回整包，体积悄悄回涨 26 MB | 中 | CI 断言轻量包不含 `tools/` |
| 分支构建写进死下载地址 | 中 | 兜底清单只在 tag 构建生成；分支保持空清单 |
| 惰性导入漏进 `--hidden-import` | 中（打包后才暴露） | `--selftest` 逐个 import；CI 必跑 |
| `ComponentsOnDemand` 零值与「用户关掉」混淆 | 低 | 用 `*bool`，nil 才取默认 |

## 13. 验收（`--selftest` 矩阵）

本机实测四种形态：

| 形态 | 爬取程序 | 下载器 | 退出码 |
|---|---|---|---|
| 轻量包，未装组件 | 未找到（或开发模式） | 未找到 | 1 |
| 轻量包，已装组件 | 组件来源 | 组件来源 | 0 |
| 整包 | 随包 | 随包 | 0 |
| 旧扁平布局（v0.3.0 zip） | 程序同级目录 | 程序同级目录 | 0 |

「来源」这一列是排查「装了却不生效」的唯一线索，必须先于 UI 正确。

另外两项可离线跑的验证：

- `go test ./internal/component`：组件管理的 20+ 条回归（符号链接越界、取消的
  回滚、同版本换包、指针损坏不误删…）。整包移植过来后全绿，无需改动。
- `JABGRAB_TEST_COMPONENT_ZIP=<真实组件包> go test ./internal/component -run RealPackage`：
  用真实的 20 MB 发布包跑一遍「安装 → 逐文件校验 → 执行 `--version` → `--selftest`」。
