# grabber 多维优化探测

审查日期：2026-11-27（Asia/Shanghai）。基线：`0935811`，初始工作区无未提交改动。参考结构统一提交 `c8263db`、后续 Windows/配置修正及当前 vanilla JS 界面。界面采用独立前端实现，不因视觉统一就假设它与其他三仓共享 React 运行时。

覆盖爬取/下载生命周期、完成判定、取消、去重索引、自动清理、UI 进度、组件打包和本地测试。本次审计只交付文档；假下载器、媒体占位文件均在临时目录中，无真实网络下载和用户媒体删除，临时测试源码已清理。文档交付后当天已按结论改代码，改法见「落地状态」。

## 优先级与建议顺序

P1 为成功判定、数据清理或进程无法收尾的问题；P2 为竞态、性能和发布可靠性改进。

| 编号 | 优先级 | 问题 | 证据状态 | 工作量 |
| --- | --- | --- | --- | --- |
| GR-01 | P1 | 下载中标记阻断新成品识别，找不到文件仍标记完成 | 假下载器端到端到 Go 流水线复现 | 中 |
| GR-02 | P1 | 先等待日志 EOF 再 Wait，父进程退出后的管道收尾可能无界等待 | 代码确认 | 中 |
| GR-03 | P2 | 停止不等待预取 goroutine 收尾，状态更新缺少运行代次 | 代码确认；竞态待定向压测 | 中 |
| GR-04 | P2 | 去重索引只保留同名首项，残件会遮住其他目录的成品 | 临时文件复现 | 小到中 |
| GR-05 | P2 | 大队列仍全量复制/传输/比较，建文件索引时持主锁做 I/O | 代码确认；规模收益待测 | 中 |
| GR-06 | P1 | 自动清理旧版依赖体积比例，且批量删除范围大于已验证范围 | 代码确认；应与 GR-01 联动 | 中 |
| GR-07 | P2 | 共享整包断言及 package-only 的产物一致性需要收紧 | Shell 机制复现、代码确认 | 中 |

先修 GR-01，并同步收紧 GR-06，随后处理 GR-02/03；GR-04/05 在正确性稳定后优化。GR-07 在下一次正式发布前处理。

## 落地状态（2026-11-27 晚）

文档交付后当天按上面这个顺序改完，改动在工作区、尚未提交。下表把「已修」和「未动」分开记，免得下次打开时把已修项当成待办又做一遍。

| 编号 | 状态 | 落地位置 | 覆盖用例 |
| --- | --- | --- | --- |
| GR-01 | 已修 | `downloadPhase`：`stampCandidates` → `producedVideo` → `writeDoneRecord`，最后才摘标记；`ClearDownloaded` 增加 `Path != ""` 复核（原先的 `incompleteDownload` 体积兜底已于 v0.9.2 移除，见本节末） | `TestDownloadPhaseCommitsProducedFile`、`…RejectsEmptyOutput`、`…IgnoresReportedTotal`、`TestProducedVideoOnlyAcceptsFreshWrites` |
| GR-02 | 已修 | `downloader.Run` 自建 `os.Pipe`，`Wait` 与读日志并行；**先等进程退出**，再对排空设 `drainGrace` 上限，超时关读端并杀进程组 | `TestRunDrainsWithinGraceWhenDescendantHoldsPipe`、`TestRunWaitsForLongProcessDespiteDrainGrace`、`TestRunNoTeardownWarnOnCleanExit` |
| GR-03 | 已修 | `runToken` 代次贯穿 `patch`/`patchProgress`/`updateIndex`/`markRemaining`；预取 goroutine 用 `WaitGroup` + `waitTimeout` 收尾 | `TestStaleRunTokenIsIgnored` |
| GR-04 | 已修 | `fileIndex` 改 `map[string][]string`，查找逐个实时校验、跳过残件继续找；与全遍历版 `findExisting` 的结果集合对照 | `TestIndexedLookupSkipsBrokenCandidate` |
| GR-05 | 部分 | 只做「索引构建移出主锁 + 查找不持锁 I/O」，其余暂缓（依据见下） | `BenchmarkStatePush` |
| GR-06 | 已修 | 清理以本次已提交的 `-C` 为前提，候选用 `plainCandidates` 全列，逐个过「可信成品 + 时长等长（ffmpeg 读容器头，3s 容差）」，信息不足即保留 | `TestDropPlainVideoDeletesAllVerifiedCandidates`、`TestDropPlainVideoKeepsPlainWhenNotVerifiable` |
| GR-07 | 部分 | 三处归档断言的 grep 管道已改 here-string（见 GR-07 段末「跨仓同步」）；package-only 版本一致性、`wails@latest`、重复函数定义未动 | `bash -n`、`shellcheck -S warning`、YAML 解析 |

GR-05 为什么只做一半：`BenchmarkStatePush` 实测单次「深拷贝全队列 + JSON 序列化」在 100 条 0.13ms、1000 条 0.99ms（660KB）、10000 条 7.1ms（14.8MB）。150ms 合流下 1000 条队列约 7ms/s CPU，为这点开销重做增量事件协议不划算；队列真上到万级再回来看这条。

## GR-01：成功应由本次实际产物决定，不能由进程退出码决定

**状态**：已修（2026-11-27），落地位置与用例见开头「落地状态」。

**依据**

- `go/internal/pipeline/pipeline.go:390-399` 下载前写 `.jabgrab-part` 标记。
- 同文件 `440-452` 在移除标记之前，调用 `existingVideo/findExistingIndexed` 寻找成品。
- `790-797,836-850`：`existingVideo` 要求 `completedVideo` 为真；没有完成记录而存在 part 标记时必定为假。
- `444-466`：找不到输出只记 debug，仍移除标记、设置 `StatusDone` 和 100%。
- `go/app.go:335-350`、`go/internal/pipeline/prune.go:25-26` 根据 done/skipped 状态清理 TXT URL，错误成功会影响后续恢复。

**已执行复现**：调用真实 `downloadPhase`，使用临时假下载器。

| 假下载器行为 | 流水线结果 |
| --- | --- |
| 创建非空 `AUDIT-001.mp4` 后退出 0 | `status=done`、`path=""`、无完成记录 |
| 不创建任何输出，直接退出 0 | 同样 `status=done`、`path=""`、无完成记录 |

第一种情况下，标记让刚产生的文件被当成残件，完成记录无法建立；标记又被删除，后续运行会按“无标记的旧成品”信任它。第二种则可能把没有下载成功的 URL 从待处理列表清掉。

**解决方案**：分开“查找既有可信成品”和“校验本次下载候选”两个入口。状态按 `downloading → validating → committed → done` 推进：找到本次实际输出、验证媒体可读性/时长/预期流，原子写完成记录，最后移除 part 标记并发布完成状态。无输出或验证失败保持 failed/待重试；标记和记录写失败不能悄悄退回旧版宽松判定。移除 TXT URL 应复核已提交的完成证据。

**验收**：新文件正常完成、退出 0 无输出、退出 0 但输出截断、取消、磁盘满、完成记录写入失败；成功项都有非空路径和可读成品，失败项留在 TXT 中，重启后仍能准确区分残件。

**后续变更（v0.9.2）**：`incompleteDownload` 已删除，`downloadPhase` 不再拿「下载器报告的总体积」比对成品体积。实测该数字在分片尚未探完时会持续递增（`jable.tv/videos/abf-403` 成品 1.94GB、报告值一路涨到 5.44GB），按 0.9 阈值卡会误判已经下完的片子。截断的判定改回「只认下载器退出码 + `producedVideo` 找到本次写出的成品」，对应用例改为 `TestDownloadPhaseIgnoresReportedTotal`。「退出 0 但无产物」仍判失败（`TestDownloadPhaseRejectsEmptyOutput`）。

## GR-02：避免在观测进程退出之前无界等待管道关闭

**状态**：已修（2026-11-27），落地位置与用例见开头「落地状态」。

**依据**

- `go/internal/downloader/downloader.go:242-244` 设置 `WaitDelay=5s`。
- 同文件 `279-301` 启动两个读协程后，先等待两个 `done`（读取到 EOF），之后才调用 `cmd.Wait()`。
- 子进程继承 stdout/stderr 后，即使下载器父进程已经退出，管道也可能继续打开。没有取消请求时，延迟执行 Wait 无法及时从父进程退出触发收尾。

**影响**：下载器进程看似结束，UI 却一直显示运行中。当前进程组取消仍可能解救这个状态，因此不能将本问题表述为“停止永远无效”。本次未在真实下载器上制造孤儿子进程，结论由管道与 Wait 顺序确认。

**解决方案**：并行观测进程退出与日志消费，并在退出/取消后设置有界排空和关闭策略。可以采用自有 `os.Pipe` 生命周期，或让 `exec.Cmd` 管理向流式解析 writer 的复制。保留有限尾部日志；不要简单把现有 `StdoutPipe` 下的 Wait 提前而不处理其关闭读端的问题，否则会丢末尾日志。无法终止的独立后代应返回收尾超时并让任务进入明确终态。

**验收**：假进程正常退出、延迟输出最后一行、后代持有管道、取消、单侧日志解析失败；都在规定上限内返回，末尾错误保留，没有遗留当前任务的受管理子进程。

## GR-03：运行代次与预取生命周期要一起结束

**状态**：已修（2026-11-27），落地位置与用例见开头「落地状态」。

**依据**

- `go/internal/pipeline/pipeline.go:198-207` 启动预取 goroutine，结果通过带缓冲 channel 返回，没有 WaitGroup/任务句柄负责最终 join。
- 同文件 `247-265,160-169`：当前下载取消后可以直接离开循环，将 runner 设置为非运行状态；未等待可能存在的预取任务。
- `315-349` 的预取阶段按队列下标更新共享状态；`1015-1021` 只校验下标范围，没有 run ID。
- `1007-1011` 的延时进度回调同样没有事件代次/序号。

**影响**：停止后立即开始新队列或单项重试，旧任务的迟到更新可能写入新队列同一位置，或旧事件覆盖新状态。现有取消进程组逻辑有价值，但取消请求发出不等同于所有 goroutine 和事件都已退出。

**解决方案**：每轮/单次重试使用唯一 run ID，patch、日志和前端事件携带代次及单调序号；校验当前任务身份后才写状态。停止先取消，等待下载、预取及定时回调完成，再允许重启/重置。收尾等待应可观测且有界，避免用全局锁包住阻塞等待。

**验收**：控制假爬取器在停止后才返回、预取还未被调度时停止、停止后立即开始新 TXT、延迟事件乱序；旧任务不修改新队列，running=false 表示受管理任务已收尾。

## GR-04：去重索引必须保留同名的所有候选

**状态**：已修（2026-11-27），落地位置与用例见开头「落地状态」。

**依据**

- `go/internal/pipeline/pipeline.go:724-741`：`buildFileIndex` 为每个文件名只保留第一次遇到的路径，建立时未验证完成状态。
- `696-719`：索引命中后只检查这一个候选，若它是残件/已消失则返回空，没有继续搜索其他目录同名文件。

**复现**：A、B 两个演员目录均有 `ABC-123.mp4`，A 带 part 标记，B 为有效旧成品。当前索引查询返回空，而同源码中的全目录 `findExisting` 正确找到 B。说明加速索引改变了已有查找语义。

**解决方案**：索引存 `filename → []candidate`，保留既定目录优先级；实时验证候选，跳过失效项后继续寻找。构建时可以保存 stat 信息减少重复访问，但不能将可能变化的“完成状态”永久缓存。下载提交、旧版删除和目录失效时统一更新索引。

**验收**：同名首项为残件/已删除/不可读、后项为成品，仍能跳过重复下载；多个扩展名和目标目录优先级保持一致。对相同临时目录树比较索引版与完整遍历版的结果集合。

## GR-05：性能优化应继续消除全队列工作，并缩短 I/O 持锁时间

**状态**：部分修（2026-11-27）：持锁 I/O 已改；增量事件、前端窗口化、日志批插按实测数据暂缓，见开头「落地状态」。

**已有能力**：`patchProgress` 已有 150 ms 合并；前端 `queueRender` 使用 rAF，`renderState` 已按行签名更新。不能再把“每个进度事件都重建全表”写成当前问题。

**剩余依据**

- `go/internal/pipeline/pipeline.go:1000-1011,1070-1080`：每次推送仍深拷贝全队列，再经 Wails 序列化。
- `go/frontend/dist/app.js:287-308`：每帧仍遍历所有 URL、逐行 JSON.stringify 签名并汇总；单行变化仍与总任务数相关。
- `go/internal/pipeline/pipeline.go:194-196,510-512`：持 `r.mu` 遍历目录建立文件索引；慢磁盘会连带阻塞 Snapshot/Stop 取得同一把锁。
- `go/frontend/dist/app.js:510-535`：日志逐条读布局、插 DOM、裁剪和滚动，批量日志会反复触发布局工作。

**解决方案**：首次发送全量快照，后续发送 `{runId,seq,itemId,changedFields}` 和累计摘要，缺帧时再请求全量；大队列使用窗口化显示。文件索引在锁外构建并支持取消，完成后在锁内校验代次并一次替换。日志按帧/批次插入，集中完成一次尺寸测量和滚动修正。

**验收**：100/1,000/10,000 任务、同频率进度和批量日志，比较事件字节、Go 分配量、前端长任务、DOM 数量与停止响应；小队列交互不回归。真实下载带宽不一定因此增加，收益主要在 UI 和进程资源。

## GR-06：恢复自动清理前，收紧对新版完整性及删除范围的判断

**状态**：已修（2026-11-27），落地位置与用例见开头「落地状态」。

**依据**

- `go/internal/pipeline/pipeline.go:564-619` 只要求字幕版文件至少是某一普通版的一半大小，就允许清理。
- `931-969` 随后遍历多个目录和扩展名删除所有匹配普通版，验证过的一份普通版不能代表所有目标。
- 清理发生在本次下载完成记录建立之前（`431-452`）。代码注释本身已经指出上游工具可能退出 0 却只留下碎片。

**边界**：GR-01 当前会阻断很多首次字幕版下载的清理流程；不能据此宣称每次下载都会误删。但是修复完成判定后，这个以体积推断完整性的路径会重新变得活跃，必须同步处理。

**解决方案**：使用 GR-01 的已提交新产物作为前提，校验新版真实时长、必要媒体流和可解码性；文件大小只能辅助判断，不能证明是完整替代。列出所有待删除路径，并对每个目标核对作品/版本身份。自动清理的范围应限定在明确可替代项，默认采用可恢复的隔离/回收策略或让用户确认候选，不按同名把所有目录的文件直接永久删除。

**验收**：高码率截断字幕版、低码率完整字幕版、不同长度版本、同名多目录、存在多个普通版；信息不足时保留旧文件，确认替代时仅处理已验证集合。测试只使用临时文件/合成媒体。

## GR-07：打包断言与产物版本必须一起验证

**状态**：部分修（2026-11-27）：grep 管道已改，其余见本节末「跨仓同步」。

**依据**

- 四仓 `common.sh/build.sh/build.ps1/package.sh/package.ps1` 与 `docs/packaging-contract.md` 已逐字比对一致；当前 `JabGrab/{App,tools/...}` 布局无需重新设计。
- `tools/package.sh:118,179` 的 `printf ... | grep -q` 在 `pipefail` 下仍可能被 SIGPIPE 误判。本机 20,000 行同构名单有匹配却返回 141。Python 整包条目多，应纳入常规验证。（2026-11-27 两处已改为 here-string，见本节末「跨仓同步」）
- `.github/workflows/release.yml:122-129` 的整包入口检查已使用 here-string，应保留；但 `132` 的标准包禁止 tools 检查仍是 `unzip | grep -q`，不能把整个 workflow 都说成已修好或都未修好。（`132` 已于 2026-11-27 一并改为 here-string）
- `tools/build.sh:29-33,89-99`：package-only 跳过 App 编译，却仍运行组件打包与内置清单生成；组件变化时旧 App 的嵌入清单不会随源 JSON 更新。
- `.github/workflows/release.yml:81-85` 使用 `wails@latest`；共享 `package.sh:83-100,135-152` 重复定义组件辅助函数。

**解决方案**：统一用名单文件或 here-string 检查归档；共享改动同步四仓，删除被后续定义覆盖的重复函数。记录 App 与组件的构建版本、平台和清单摘要，package-only 只复用相互匹配的产物；需要更新内置清单时重编 App。固定 Wails CLI 与 Go 模块版本一致，发布前运行现有 Go/Python/前端验证及小型假下载器流程。

**验收**：长名单早/晚命中、标准包误含 tools、整包缺组件、旧 App 配新清单、三平台命名和组件自述版本。未执行真实 Windows/macOS 发布前，不声称分发包已通过实机升级。

**跨仓同步：修法与实测边界已定稿（2026-11-27，本仓已落地）**

本仓三处断言已统一改成「先落名单 → here-string」：先 `names="$(unzip -Z1 "$zip")"`，再 `grep -q "…" <<<"$names"`。here-string 不经过写端进程，`grep -q` 一命中就退出也不会再顶出 141。其余三仓的 `tools/package.sh` 在改动前与本仓逐字一致，同步时可直接用本仓这份覆盖。

- 复现边界（4000 条目归档、tools 条目排在最前、总量远超管道缓冲）：旧写法 `printf '%s\n' "$names" | grep -q …` 在匹配**存在**时返回 1，即漏检；新写法返回 0。触发条件是「shell 内建 echo/printf 往管道灌大变量 + 匹配在前」，随条目数增长放大。
- 边界更正：由 `unzip -Z1` 直接产出、不经变量回灌的写法，本机未复现 141（unzip 自己承担了写端行为）；真正必现的是上一条那种内建命令灌管道的形态。`release.yml:132` 属于前一类，改它是为了消除「判定结果取决于写端是谁、缓冲多大」，不是因为已复现故障。
- 本仓实际位置：`tools/package.sh:118`（标准包含 tools 即 die，漏检方向）、`tools/package.sh:179`（整包缺组件即 die，误报方向）、`.github/workflows/release.yml:132`（标准包禁止 tools）。本仓是模板 B，三处都会执行。
- 复核：`bash -n tools/package.sh`、`shellcheck -S warning tools/package.sh`、YAML 解析均通过；here-string 在 macOS 自带 bash 3.2.57 下正常。
- 验收补充：除「长名单早/晚命中」外，各造一份 tools 在前、tools 在后的归档，确认漏检方向（本该 die 却静默通过）与误报方向（本该通过却 die）都不再出现。

## 已完成验证与限制

下表是**审计当天、改代码之前**的原始记录，保留下来做对照：GR-01 修复后，「退出 0 无输出」不再标完成，而是 failed 且保留下载标记；GR-04 修复后索引能继续命中 B 目录的成品。当前状态一律以「落地状态」为准。

| 检查 | 结果 |
| --- | --- |
| `go/` 下 `go test ./...` | 退出码 0，本机摘要 167 passed / 12 packages；真实下载与真实组件包用例受环境变量控制，不等同于本次已执行 |
| `go/` 下 `go vet ./...` | 退出码 0 |
| `python/` 下 `uv run pytest -q` | 退出码 0；RTK 首次错误汇总成 No tests collected，随后 `rtk bypass uv run pytest -q` 原始输出确认为测试完成到 100% |
| `go/frontend/` 下 `node --test tests/pure.test.cjs` | 11 passed |
| 临时 `TestParentAuditCompletion` | 有输出与无输出的退出 0 两种情况均 done、空路径、无完成记录 |
| 临时 `TestParentAuditDuplicateIndex` | 索引漏掉 B 目录有效成品，完整遍历能找到 |
| `bash -n tools/*.sh`、四仓模板比较 | 通过、一致；共享长名单管道实测返回 141 |

未执行真实站点爬取、验证码、实际 HLS 下载、真实媒体删除、跨进程取消压力测试、Windows 桌面 UI 和正式组件安装。性能判断基于代码路径和数据规模，没有把这些本地测试当作生产环境基准。
