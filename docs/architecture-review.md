# JabGrab 架构与设计评审

> 评审日期：2026-11-23 · 基于 main 分支 `0eb5d16`
> 范围：目录结构、整体架构、Go 后端、Python 侧车、前端、前后端交互、性能、UI/UX、工程化。
> 文中所有问题均附源码位置，按文末优先级表排序实施即可。

---

## 1. 项目概览与目录结构

```
grabber/
├── go/                        Wails v2 桌面外壳（Go ≥ 1.27）
│   ├── main.go                入口，--selftest 短路
│   ├── app.go                 前端绑定层：Boot / Start / Stop / SaveConfig …
│   ├── components.go          组件服务导出层（564 行）
│   ├── internal/
│   │   ├── config/            配置读写与站点匹配
│   │   ├── locate/            三件套六级定位链
│   │   ├── scraper/           exec Python 侧车，解析单行 JSON
│   │   ├── downloader/        拼 N_m3u8DL-RE 参数，解析进度帧
│   │   ├── pipeline/          主流程（串行并发 1）+ txt 清理
│   │   ├── component/         清单 / 下载 / 校验 / 安装 / 跨进程锁
│   │   ├── player|reveal|proc|appearance/   平台适配小模块
│   ├── frontend/dist/         手写 HTML/CSS/JS，embed 进二进制
│   └── third_party/           内置 N_m3u8DL-RE（arm64 / win-x64）
├── python/src/jab_scrape/     SeleniumBase 抓取侧车（PyInstaller onedir）
├── scripts/                   构建 / 打包 / 拉上游二进制
├── docs/                      设计文档
└── .github/workflows/         tag 触发 14 资产发布
```

**结构评价：总体优秀。** 分层清晰（绑定层 → internal 领域模块 → 外部进程），跨平台代码一律
`_unix.go / _windows.go` 文件级拆分而非运行时 if，`.gitignore` 覆盖完整（84 个跟踪文件，
运行时残留全部隔离），Go 侧测试密度高且含回归测试。这套结构对一个两人规模的项目是超预期的。

可改进的结构问题：

| # | 问题 | 建议 |
|---|---|---|
| S1 | **前端源码即产物**：手写代码直接放在 `frontend/dist/`，与 `wailsjs/` 生成物同区，没有 src/build 之分，也没有任何 lint / 类型检查 | 不必上框架，但建议 `frontend/src/` + 一条 esbuild 拷贝/压缩命令产出 dist；最低成本方案是给 JS 加 JSDoc + `tsc --checkJs --noEmit`，配合 eslint。805 行的 app.js 已接近裸 JS 可维护上限 |
| S2 | **运行时产物落在仓库根**：`Logs/`、`N_m3u8DL-RE`、`download.txt`、`go/third_party/Logs/` 散在工作区（虽已 ignore） | 开发期运行目录统一指到 `.dev/` 或 `JABGRAB_CONFIG_DIR` 指向的临时目录，保持根目录只有源码与脚本 |
| S3 | `app.go` + `components.go` 同为 package main 的绑定层，后者 564 行且含清单缓存等业务逻辑 | 把 componentService 下沉到 `internal/component` 的 facade，main 只留 DTO 转换与事件转发，与其余模块的分层保持一致 |
| S4 | 前端零测试。Go/Python 都有单测，唯独 UI 逻辑（`renderState` 的进度汇总、`normalize`、prune 文案分支）无任何验证 | 把纯函数（进度汇总、humanSize、normalize）抽出后用 vitest/node:test 覆盖，成本极低收益明显 |

---

## 2. 整体架构

数据流：`txt → pipeline（串行）→ jab-scrape（浏览器抓 m3u8）→ N_m3u8DL-RE（下载）→ 按演员归档`，
进度经 Wails 事件推给前端。三件套（爬取器 / 下载器 / ffmpeg）由六级定位链解析，组件按需下载。

**值得肯定的设计**（保持，不要动）：

- **组件管理是全项目安全水位最高的模块**：sha256 + 字节数双校验、强制 https 且重定向逐跳校验、
  zip 解压防目录穿越与符号链接逃逸、staging → 校验 → rename → 写指针的可回滚安装、
  进程内信号量 + 跨进程 flock 双层锁（`internal/component/install.go`、`fetch.go`、`lock.go`）。
- **进度帧解析的工程细节**：正则按单位锚定取最后匹配兼容粘连帧，`splitLines/flushCut` 处理
  管道下无换行的 4KB 增量输出（`internal/downloader/downloader.go:377-471`）。
- **prune 的原子写**：临时文件 + rename，保留 CRLF/BOM/权限（`internal/pipeline/prune.go:88-119`）。
- **配置损坏不覆盖原文件**（`app.go:78-90`）、`--selftest` 产物级自检、注释即事故记录的风格。
- 轻量包 / 整包 / 组件包拆分发布，换爬取规则不必重发 App——这是正确的分发架构。

**架构层面的两个根本性瓶颈**（详见 §6 性能）：

1. **全串行流水**：爬取（开浏览器 + 固定 sleep）与下载互相等待，单条间隔至少 15s 纯等待。
2. **全量快照广播**：每个进度帧深拷贝并推送整个任务列表，前端再整表重建 innerHTML。
   列表小（几十条）时无感，但这是 O(任务数 × 帧率) 的放大链路，两端都没有节流。

---

## 3. 后端设计问题

### B1（正确性）失败任务的错误信息可能被进度条吞掉 —— 前后端联合 bug

`app.js:239` 中 `showBar = st === "downloading" || st === "done" || pct > 0`。
下载到 40% 失败时 `pct > 0` 成立，该行渲染进度条而非 `it.message`，**用户看不到失败原因**，
只能去日志面板翻。失败态应无条件展示 message（进度条可保留但置灰）。

### B2（健壮性）config.Save 非原子写

`internal/config/config.go:187` 直接 `os.WriteFile`。同项目的 prune 已经做了 temp+rename，
config 反而没有——崩溃/断电时机不巧会损坏配置，而 startup 对损坏配置的策略是"用默认值运行"，
用户会静默丢失全部设置。复用 prune 的原子写即可，十行改动。

### B3（健壮性）"已存在"判定过弱，残件会被当成品

`pipeline.go` 按文件名 + size>0 判定跳过（约 L537-545）。上次中断留下的非空残件会被标
`已存在` 并给出播放按钮，且 prune 会把这条 URL 从 txt 删掉——**残件永久顶替了完整下载**。
建议：下载完成时写 sidecar 记录（`.jabgrab.json`：url、大小、完成时间），跳过判定优先查记录；
无记录时至少用 ffprobe 验时长或对比清单大小。

### B4（健壮性）downloader WaitDelay=0，kill 失败则 Wait 永久挂起

`internal/downloader/downloader.go:242`。scraper 侧设了 5s WaitDelay，downloader 没有；
读协程出错主动 kill 是对的，但 kill 失败（进程僵死、句柄被子进程持有）时 `cmd.Wait()`
无兜底超时，整条流水线卡死且"停止"也救不回。设 `WaitDelay = 5*time.Second` 对齐 scraper。

### B5（一致性）重复实现

- ffmpeg 查找/校验逻辑在 pipeline（L186-191）、downloader（L223-230）、`app.go:281-287` 三处各写一遍，
  校验口径将来容易漂移。收敛到 locate 提供 `RequireFFmpeg(cfg, dlrPath)` 单一入口。
- stderr ring buffer 在 scraper 与 downloader 各一份，抽到 `internal/proc` 或 `internal/ring`。

### B6（可取消性）组件后台操作用 context.Background()

`install.go` 的 Remove（L664）/ CleanupStale（L735）/ Verify（L827）不可取消，
退出应用时这些操作可能还在扫盘。startup 时传入 app ctx 派生的 context 即可。

### B7（小项）

- `findExisting` 每条任务扫保存目录全部一级子目录（演员目录多时 O(n×m)）；一轮开始时建一次
  `文件名 → 路径` 索引，本轮内复用（注意本轮新落盘的文件要进索引）。
- `locate.FFmpeg` 配置路径无效时静默返回空串（locate.go:256），与 `locate.Dlr` 的"配置坏则报错"
  口径不一致，用户填错 ffmpeg 路径得不到明确提示。
- Python 侧 `fetch_html` 失败只回"获取网页源码失败"，真实异常仅在 stderr（scraper.py:86-88）；
  把异常类名带进 stdout JSON 的 error 字段，前端失败原因才有信息量。

---

## 4. 前后端交互

现状：事件为主（`pipeline:state` / `pipeline:log` / `component:*`），`State()` 轮询兜底，
`Boot()` 一次拿全首屏数据。方向正确，问题在粒度与频率：

### I1（核心）状态推送无节流 + 全量快照

链路：dl-re 每输出一帧进度 → `pipeline.go:322-331` 回调 → `patch()` 深拷贝整个 State
（`pipeline.go:684-693`）→ `EventsEmit("pipeline:state")` JSON 序列化全量 items →
前端 `renderState` 整表 `innerHTML` 重建（`app.js:289`）。

组件安装进度知道按 100ms 限流（`app.go:64-68` 注释），**任务进度反而没有**。建议双端各改一半：

- 后端：进度类 patch 合并节流（100–250ms 内的多次进度更新只 emit 一次；状态跃迁
  scraping→downloading→done 立即 emit）。进一步可改增量事件 `pipeline:item`（只发变化的那条），
  全量快照仅在 Start/Stop/Reset 时发。
- 前端：见 F1。

### I2 Boot/SaveConfig 同步做重活

`Boot()` 内 `dlrVersion` 首次要起进程跑 `--version`（3s 超时，`app.go:374-402`），
`SaveConfig` 每次保存都完整重跑一遍 Boot。下载器二进制在慢盘/被杀软扫描时首屏会白等。
建议：Boot 先返回不含 version 的结果，version 异步补一个 `dlr:version` 事件；
或 SaveConfig 只返回 issues + config，环境探测结果复用缓存。

### I3 前端错误反馈通道单一

播放失败、保存失败、Start 被拒都只写日志面板或 banner（`app.js:351-369`）。日志面板可能被
拖到很矮或在设置页根本看不见。建议加一个轻量 toast（纯 CSS + 一个容器 div 即可），
错误类信息 toast + 日志双写。

### I4 危险操作无确认

「清除已下载」直接改写用户的 txt 文件（`app.go:308-343`）。虽保留失败行且设计周到，
但误点即改盘上文件。建议首次点击弹确认（记住选择），或 prune 前在配置目录留一份
`txt.bak` 支持撤销。

---

## 5. 前端设计

### F1（核心）整表 innerHTML 重建

`renderState`（`app.js:220-322`）每帧重建全部行：

- 滚动位置、文本选中、按钮 hover/active 态每帧丢失；下载中想选中 URL 复制几乎不可能。
- 行数多时每帧全表 reflow，与 I1 叠加成双重放大。

建议（不引框架的前提下）：按 `url` 作 key 做行级 diff——行集合不变时只更新变化行的
状态/进度/速度单元格（`querySelector` 定位 + textContent/style 写入）；行集合变化才重建。
配合 `requestAnimationFrame` 合帧，渲染频率自然封顶 60fps。这一项与 I1 是同一问题的两端，
**建议一起做，是全项目性价比最高的优化**。

### F2 设置页表单状态管理脆弱

`draft` 只在 boot/save 时更新，`collectConfig` 保存时从 DOM 全量读回（`app.js:635-663`）。
输入框与 draft 长期不同步，切 tab、组件刷新重渲染都可能造成"改了没保存就丢"。建议：
input 事件统一写回 draft（单向数据流），渲染只从 draft 出；加 dirty 标记，
保存栏 sticky 置底并显示"有未保存修改"。

### F3 renderComponents 每次进度事件全量重建 + 重复绑事件

`app.js:67-74` 每 100ms 进度事件触发 `renderComponents()` 全量 innerHTML + 重新 addEventListener
（`app.js:494-513`）。两个组件卡片问题不大，但安装中按钮区域每 100ms 重建一次，
点击目标会闪。进度更新应只写对应卡片的 bar 宽度与文案节点；事件委托到容器绑一次
（任务列表已经这么做了，`app.js:339-349`，组件列表照抄即可）。

### F4 小项

- `normalize` 用 `JSON.parse(JSON.stringify())` 深拷贝，可用 `structuredClone`。
- `intVal` 对超范围值静默回落默认（`app.js:665-668`），用户填 500 线程被悄悄改回 25 无提示；
  至少 clamp 到 min/max 并回写输入框。
- 日志面板无级别过滤/搜索/导出，2000 行裁剪已有，但排查失败时只能肉眼扫。加一行
  「全部/警告/错误」过滤 chips + 关键字输入即可，数据都在 DOM 里。

---

## 6. 性能专项（按收益排序）

### P1 Python 侧固定 sleep 是最大的单条延迟来源

`scraper.py:78,84` 固定 `sleep(PAGE_WAIT_SEC)` + `sleep(CAPTCHA_WAIT_SEC)`（合计约 15s），
不看页面实际就绪状态。100 条任务 = 25 分钟纯等待。改成条件等待
（`wait_for_element` / 轮询 hlsUrl 出现，上限保留现值），正常页面 2–4s 即可返回，
**整轮耗时预计缩短 30–50%**。这是不改架构就能拿到的最大收益。

### P2 爬取与下载流水化（prefetch 1）

当前严格串行：爬完 → 下完 → 再爬下一条。下载一条通常几十秒到几分钟，期间完全可以把下一条
的 m3u8 抓好。pipeline 加一个深度 1 的预取 goroutine（下载第 N 条时爬第 N+1 条），
失败不阻断主流。注意：m3u8 地址有时效性，预取深度不要超过 1；停止时两个进程组都要杀。
与 P1 叠加后，单条间隔趋近于零。

### P3 事件与渲染链路节流

见 I1 + F1。当前帧率下几十条任务无感，但属于"列表一大就崩"的隐性负债。

### P4 浏览器进程复用（远期）

每条 URL 冷启动一次 Chromium（PyInstaller onedir + seleniumbase UC 模式），启动本身 2–5s。
远期可把侧车改成常驻 daemon（stdin 逐行喂 URL，stdout 逐行回 JSON），浏览器实例跨条复用。
收益明显但改动面大（进程生命周期、崩溃恢复、定位链、selftest 全要跟着动），
建议在 P1/P2 榨干后再评估。

---

## 7. 页面美观与 UX

现有基础很好：亮暗双主题全 token 化（`style.css:1-62`）、首帧主题脚本防白闪、
分隔条带完整 aria 与键盘支持、日志裁剪保持滚动偏移。以下是提升建议：

### U1 表格 9 列过挤

min-width 900px 下 9 列，名称列被压得最狠。「播放」「文件夹」两列合并为一个「操作」列
（两个 icon button），「状态」「进度」可合并（状态 tag 放进度条上方一行），
省出的宽度全部给名称/URL。失败原因（B1 修复后）也才有地方放。

### U2 失败行的视觉权重不足

失败仅整行淡红底（`row-failed`），在长列表里不显眼。建议：状态 tag 用实心 danger 底、
行左缘 3px danger 色条、message 以 danger 文本直接展示在名称下方。
配合任务级「重试」按钮（单条重跑，现在只能整轮重来或手改 txt）。

### U3 总进度语义模糊

`app.js:292-307` 把 failed/cancelled 也计入"已完成"分子，总进度条 100% 时可能一半是失败。
建议分段进度条（成功绿 / 失败红 / 剩余灰，flex 三段即可），summary 文案分开计数。

### U4 任务行缺少常用操作

复制 URL、单条重试、从本轮队列移除——都是下载器 UI 的标配，目前一个都没有。
事件委托已就位，加 `data-action` 分支成本很低。

### U5 设置页信息架构

五张 card 纵向一屏放不下，保存按钮在最底部。建议：左侧锚点子导航（路径/播放/组件/站点/参数）
或 card 可折叠；保存栏 sticky（配合 F2 的 dirty 提示）。
「下载参数」对多数用户是低频项，可默认折叠成"高级"。

### U6 细节打磨

- 空状态只有一行灰字，可加一个引导按钮（直达"选择 txt"）。
- 组件卡片安装中无取消按钮（后端锁支持取消，前端没暴露）。
- 13px 正文在 4K 屏偏小，可给 `html { font-size }` 挂一档设置或跟随系统缩放。
- 顶栏 envBadge 只有"n 项待处理"，点击应能展开/滚动到 issueBanner（现在 banner 在视口外时无从点起）。

---

## 8. 工程化

已达标：Go 单测 + 回归测试、Python pytest + ruff、三平台原生 CI、产物级 selftest 断言来源、
manifest 平台齐全性校验、SHA256SUMS。缺口只有前端（见 S1/S4）与两处：

- `go/third_party/Logs/` 是运行残留混进了源码目录（已 ignore，但建议运行目录外移，见 S2）。
- README 与实际行为有一处口径差：README 说"不重试"，指任务级；dl-re 分片级重试 20 次是开的。
  建议在 README 注明两层重试的区别，避免误解。

---

## 9. 优先级路线图

| 级别 | 项 | 内容 | 预估改动量 |
|---|---|---|---|
| **P0 正确性** | B1 | 失败行错误信息被进度条吞掉 | 前端 ~10 行 |
| | B2 | config.Save 原子写 | Go ~15 行 |
| | B3 | 残件误判"已存在"（sidecar 完成记录） | Go ~80 行 + 测试 |
| | B4 | downloader WaitDelay 兜底 | 1 行 |
| **P1 性能/体验** | P1 | Python 条件等待替代固定 sleep | Python ~30 行 |
| | I1+F1 | 状态节流 + 行级 diff 渲染 | Go ~40 行，JS ~120 行 |
| | I3 | toast 错误反馈 | JS/CSS ~80 行 |
| | U1+U2 | 列合并 + 失败行强化 | HTML/JS/CSS 中等 |
| **P2 结构/打磨** | P2 | 爬取预取流水化 | Go ~100 行 + 测试 |
| | I4 | prune 确认/备份 | 小 |
| | F2 | 设置页单向数据流 + sticky 保存栏 | JS 中等 |
| | S1/S4 | 前端 lint/类型检查 + 纯函数测试 | 工程配置 |
| | B5/B6/B7 | 重复收敛、可取消、索引、错误透传 | 各 ~20-50 行 |
| **P3 远期** | P4 | 侧车 daemon 化浏览器复用 | 大，先评估 |
| | U3-U6 | 分段进度、任务操作、设置页 IA、细节 | 渐进 |

**一句话总结**：这套代码的防御性工程（组件安全、进程管理、原子 prune、定位链）已是高水位，
短板集中在三处——串行流水的吞吐、全量快照 × 整表重建的渲染链路、以及几个"残件/损坏配置"
级别的正确性缝隙。先修 P0 四项，再做 sleep 条件化 + 渲染节流，项目整体质量就能再上一个台阶。