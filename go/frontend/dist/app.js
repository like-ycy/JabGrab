/* JabGrab 前端：vanilla JS，无框架。
   后端方法通过 Wails 注入的 window.go.main.App 调用。 */

const api = () => window.go?.main?.App;

const STATUS_TEXT = {
    pending: "待处理",
    scraping: "爬取中",
    downloading: "下载中",
    done: "完成",
    skipped: "已跳过",
    failed: "失败",
    cancelled: "已取消",
};

const el = (id) => document.getElementById(id);

let draft = null; // 设置页正在编辑的配置副本
// 外部组件的最近一次状态。它同时被设置页的卡片与任务页的提示条使用，
// 所以放在模块级：两个视图渲染同源数据，不会出现「设置页说没装、任务页说装好了」。
let components = [];
let componentsDir = "";
let lastState = { running: false, index: 0, total: 0, items: [] };
let logCount = 0;
let followLog = true;
let logScroll = { top: 0, remainder: 0 };
const LOG_LIMIT = 2000;

/* ---------- 启动 ---------- */

async function boot() {
    bindTabs();
    bindButtons();
    bindTaskActions();
    bindPaneDivider();

    if (!api()) {
        appendLog({ level: "error", text: "未检测到后端绑定，请通过 JabGrab 程序启动本界面。" });
        el("envBadge").textContent = "后端未连接";
        el("envBadge").className = "env bad";
        return;
    }

    try {
        const info = await api().Boot();
        draft = normalize(info.config);
        componentsDir = info.componentsDir || "";
        setComponents(info.components || [], info);
        renderEnv(info);
        renderSettings(draft);
        renderState(info.state || lastState);
        appendLog({
            level: "info",
            text: info.dlrVersion
                ? `下载器就绪：${info.dlrVersion}`
                : "等待配置：请到设置页确认路径与爬取命令",
        });
    } catch (err) {
        appendLog({ level: "error", text: "初始化失败: " + err });
    }

    window.runtime.EventsOn("pipeline:state", (s) => renderState(s));
    window.runtime.EventsOn("pipeline:log", (l) => appendLog(l));

    // 组件事件：进度按 100ms 限流后推来，done/failed 是终态。
    // 刷新（refreshed）只表示「清单拉回来了」，用 ListComponents 重取一次最稳。
    window.runtime.EventsOn("component:progress", (p) => {
        const c = components.find((x) => x.name === p.name);
        if (!c) return;
        c.installing = true;
        c.progressDone = p.done;
        c.progressTotal = p.total;
        renderComponents();
    });
    window.runtime.EventsOn("component:done", (p) => {
        appendLog({ level: "info", text: `组件 ${p.name} ${p.version} 已安装` });
        refreshComponents();
    });
    window.runtime.EventsOn("component:failed", (p) => {
        appendLog({ level: "error", text: `组件 ${p.name} 安装失败：${p.message}` });
        refreshComponents();
    });
    window.runtime.EventsOn("component:refreshed", () => refreshComponents());
}

function normalize(cfg) {
    const c = JSON.parse(JSON.stringify(cfg || {}));
    c.saveDir = c.saveDir || "";
    c.txtPath = c.txtPath || "";
    c.recentTxt = c.recentTxt || [];
    c.dlrPath = c.dlrPath || "";
    c.pythonCmd = c.pythonCmd || "";
    c.ffmpegPath = c.ffmpegPath || "";
    c.playerPath = c.playerPath || "";
    c.componentMirror = c.componentMirror || "";
    c.componentsOnDemand = c.componentsOnDemand !== false;
    c.sites = c.sites || {};
    c.download = c.download || {};
    c.scrapeTimeoutSec = c.scrapeTimeoutSec || 180;
    return c;
}

/* ---------- 视图切换 ---------- */

function bindTabs() {
    document.querySelectorAll(".tab").forEach((btn) => {
        btn.addEventListener("click", () => {
            const log = el("log");
            if (log.clientHeight > 0) followLog = logAtBottom(log);
            document.querySelectorAll(".tab").forEach((b) => b.classList.remove("active"));
            document.querySelectorAll(".view").forEach((v) => v.classList.remove("active"));
            btn.classList.add("active");
            el("view-" + btn.dataset.view).classList.add("active");
            if (log.clientHeight > 0 && followLog) log.scrollTop = log.scrollHeight;
        });
    });
}

/* ---------- 列表与日志分隔条 ---------- */

function bindPaneDivider() {
    const panes = el("taskPanes");
    const divider = el("paneDivider");
    const logPanel = el("logPanel");
    let logHeight = 190;
    let drag = null;

    const resize = (height) => {
        if (panes.clientHeight === 0) return;
        const available = panes.clientHeight - divider.offsetHeight;
        const max = Math.max(0, available - 120);
        const min = Math.min(90, max);
        logHeight = Math.max(min, Math.min(max, height));
        logPanel.style.flexBasis = logHeight + "px";
        divider.setAttribute("aria-valuemin", String(Math.round(min)));
        divider.setAttribute("aria-valuemax", String(Math.round(max)));
        divider.setAttribute("aria-valuenow", String(Math.round(logHeight)));
        divider.setAttribute("aria-valuetext", `运行日志高度 ${Math.round(logHeight)} 像素`);
    };

    divider.addEventListener("pointerdown", (event) => {
        if (event.button !== 0 || drag) return;
        event.preventDefault();
        divider.focus();
        drag = { id: event.pointerId, y: event.clientY, height: logPanel.offsetHeight };
        divider.setPointerCapture(event.pointerId);
        document.body.classList.add("resizing");
    });
    divider.addEventListener("pointermove", (event) => {
        if (!drag || drag.id !== event.pointerId) return;
        resize(drag.height + drag.y - event.clientY);
    });
    const stopDrag = (event) => {
        if (!drag || drag.id !== event.pointerId) return;
        drag = null;
        document.body.classList.remove("resizing");
        if (divider.hasPointerCapture(event.pointerId)) divider.releasePointerCapture(event.pointerId);
    };
    divider.addEventListener("pointerup", stopDrag);
    divider.addEventListener("pointercancel", stopDrag);
    divider.addEventListener("lostpointercapture", stopDrag);
    divider.addEventListener("keydown", (event) => {
        if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
        event.preventDefault();
        resize(logHeight + (event.key === "ArrowUp" ? 20 : -20));
    });
    new ResizeObserver(() => resize(logHeight)).observe(panes);
    resize(logHeight);
}

/* ---------- 环境与自检 ---------- */

function renderEnv(info) {
    const badge = el("envBadge");
    const issues = info.issues || [];

    if (issues.length === 0) {
        badge.textContent = info.dlrVersion || "环境就绪";
        badge.className = "env ok";
        el("issueBanner").classList.add("hidden");
    } else {
        badge.textContent = `${issues.length} 项待处理`;
        badge.className = "env bad";
        const banner = el("issueBanner");
        banner.textContent = issues.map((s) => "· " + s).join("\n");
        banner.classList.remove("hidden");
    }

    el("txtPath").textContent = info.config.txtPath || "未选择 txt 文件";
    el("txtPath").classList.toggle("set", !!info.config.txtPath);

    el("dlrHint").textContent = info.dlrPath
        ? `${info.dlrSource}：${info.dlrPath}`
        : "未找到 N_m3u8DL-RE";
    el("dlrHint").className = "hint" + (info.dlrPath ? "" : " bad");

    el("scrapeHint").textContent = info.scrapeErr
        ? info.scrapeErr
        : `实际执行：${(info.scrapeCmd || []).join(" ")}（${info.scrapeSource || "未知来源"}）`;
    el("scrapeHint").className = "hint" + (info.scrapeErr ? " bad" : "");

    if (el("ffmpegHint")) {
        el("ffmpegHint").textContent = info.ffmpegPath
            ? `${info.ffmpegSource}：${info.ffmpegPath}`
            : "未找到 ffmpeg —— 下载器启动时会拒绝运行";
        el("ffmpegHint").className = "hint" + (info.ffmpegPath ? "" : " bad");
    }

    if (el("playerHint")) {
        const custom = (info.config && info.config.playerPath) || "";
        el("playerHint").textContent = custom
            ? "行内「播放」按钮走这个播放器，成品路径作为唯一参数传给它"
            : "留空则交给系统默认播放器（双击文件时打开的那个）；Windows 上可填 PotPlayerMini64.exe";
        el("playerHint").className = "hint";
    }
}

/* ---------- 任务状态渲染 ---------- */

function renderState(s) {
    if (!s) return;
    lastState = s;

    const items = s.items || [];
    const body = el("taskBody");

    if (items.length === 0) {
        body.innerHTML = '<tr class="empty"><td colspan="8">没有可处理的任务，请选择 txt 并确认已启用站点</td></tr>';
        setOverall(0);
        el("summaryText").textContent = "等待开始";
        el("summaryDetail").textContent = "";
        setRunning(!!s.running);
        return;
    }

    const html = items.map((it, i) => {
        const st = it.status || "pending";
        const pct = st === "done" ? 100 : Number(it.percent || 0);
        const showBar = st === "downloading" || st === "done" || pct > 0;
        const name = it.name || "";
        const msg = it.message || "";

        const progress = showBar
            ? `<div class="progress-cell">
                   <div class="bar small"><i style="width:${Math.min(100, pct).toFixed(1)}%"></i></div>
                   <div class="progress-meta">
                       <span>${pct.toFixed(1)}%</span>
                       <span>${it.downloaded ? it.downloaded + " / " + (it.totalSize || "?") : ""}</span>
                   </div>
               </div>`
            : `<span class="hint">${escapeHtml(msg)}</span>`;

        const speed = it.speed ? `${it.speed}${it.eta ? " · " + it.eta : ""}` : "";

        // 只有下好的（完成 / 已存在）才有成品路径；其它状态留个空单元格，
        // 免得整列按钮在下载途中点不动还闪来闪去。
        const playCell = it.path
            ? `<button class="play-btn" data-play="${escapeHtml(it.path)}"
                       title="播放 ${escapeHtml(it.path)}">▶ 播放</button>`
            : "";

        // 演员名在爬取完成后才有；取不到时留占位符，避免整列空白看不出区别。
        const actors = Array.isArray(it.actors) ? it.actors.filter(Boolean) : [];
        const actorText = actors.join("、");
        const actorCell = actorText
            ? `<span class="actor" title="${escapeHtml(actorText)}">${escapeHtml(actorText)}</span>`
            : '<span class="hint">—</span>';

        return `<tr class="${st === "failed" ? "row-failed" : ""}">
            <td class="col-idx">${i + 1}</td>
            <td class="cell-name">
                <div class="name">${escapeHtml(name || "—")}</div>
                <div class="url">${escapeHtml(it.url || "")}</div>
            </td>
            <td class="col-actor cell-actor">${actorCell}</td>
            <td class="col-site"><span class="tag">${escapeHtml(it.site || "-")}</span></td>
            <td class="col-status"><span class="tag ${st}">${STATUS_TEXT[st] || st}</span></td>
            <td class="col-progress">${progress}</td>
            <td class="col-speed speed-cell">${escapeHtml(speed)}</td>
            <td class="col-play">${playCell}</td>
        </tr>`;
    });

    body.innerHTML = html.join("");

    // 总体进度：已结束的按 100%，当前下载中按实际百分比
    let acc = 0;
    let finished = 0;
    let current = null;
    for (const it of items) {
        const st = it.status || "pending";
        if (["done", "skipped"].includes(st)) {
            acc += 1;
            finished += 1;
        } else if (st === "failed" || st === "cancelled") {
            acc += 1;
        } else if (st === "downloading") {
            acc += Math.min(1, (it.percent || 0) / 100);
            current = it;
        }
    }
    setOverall((acc / items.length) * 100);

    const running = !!s.running;
    setRunning(running);

    if (running) {
        el("summaryText").textContent = `第 ${s.index} / ${s.total} 条`;
        el("summaryDetail").textContent = current
            ? `${current.name || ""} · ${(current.percent || 0).toFixed(1)}%`
            : "处理中…";
    } else {
        const failed = items.filter((i) => i.status === "failed").length;
        el("summaryText").textContent = `共 ${s.total} 条，完成 ${finished}${failed ? `，失败 ${failed}` : ""}`;
        el("summaryDetail").textContent = finished + failed === s.total && s.total > 0 ? "本轮结束" : "";
    }
}

function setOverall(pct) {
    el("overallBar").style.width = Math.max(0, Math.min(100, pct)).toFixed(1) + "%";
}

function setRunning(running) {
    el("btnStart").disabled = running;
    el("btnStop").disabled = !running;
    el("btnPickTxt").disabled = running;
    // 运行中不能改 txt：这一轮还在往里写状态，删完列表与文件必然对不上。
    el("btnPrune").disabled = running;
}

/* ---------- 播放 ---------- */

// 列表每次状态更新都重建 innerHTML，按钮是一次性的，所以用事件委托绑一次。
function bindTaskActions() {
    el("taskBody").addEventListener("click", (event) => {
        const btn = event.target.closest("[data-play]");
        if (btn) playFile(btn.dataset.play);
    });
}

async function playFile(path) {
    if (!api()) return;
    try {
        await api().PlayFile(path);
        appendLog({ level: "info", text: `已交给播放器：${baseName(path)}` });
    } catch (err) {
        appendLog({ level: "error", text: "播放失败: " + err });
    }
}

/* ---------- 日志 ---------- */

function logAtBottom(log) {
    return log.scrollHeight - log.clientHeight - log.scrollTop <= 24;
}

function appendLog(line) {
    const pre = el("log");
    const visible = pre.clientHeight > 0;
    const scrollTop = pre.scrollTop;
    if (visible) followLog = logAtBottom(pre);
    if (scrollTop !== logScroll.top) logScroll.remainder = 0;
    const div = document.createElement("div");
    div.className = "l-" + (line.level || "info");
    div.textContent = line.text || "";
    pre.appendChild(div);
    logCount++;

    let removedHeight = 0;
    while (logCount > LOG_LIMIT && pre.firstChild) {
        removedHeight += pre.firstChild.getBoundingClientRect().height;
        pre.removeChild(pre.firstChild);
        logCount--;
    }
    if (visible) {
        // 部分 WebView 的 scrollTop 会取整，保留小数余量，避免持续裁剪时累积偏移。
        const target = followLog ? pre.scrollHeight : Math.max(0, scrollTop + logScroll.remainder - removedHeight);
        pre.scrollTop = target;
        logScroll = { top: pre.scrollTop, remainder: followLog ? 0 : target - pre.scrollTop };
    }
}

/* ---------- 外部组件 ---------- */

// 组件状态 → 徽标文案与配色。key 与后端 component.State.String() 一一对应。
const COMPONENT_STATE = {
    ready: { text: "已就绪", cls: "done" },
    missing: { text: "未安装", cls: "failed" },
    outdated: { text: "可更新", cls: "scraping" },
    corrupt: { text: "文件不完整", cls: "failed" },
    incompatible: { text: "与程序不兼容", cls: "failed" },
};

function setComponents(list, info) {
    components = Array.isArray(list) ? list : [];
    if (info && info.componentsDir) componentsDir = info.componentsDir;
    renderComponents();
}

async function refreshComponents() {
    if (!api()) return;
    try {
        setComponents(await api().ListComponents());
    } catch (err) {
        appendLog({ level: "warn", text: "刷新组件状态失败: " + err });
    }
}

function humanSize(bytes) {
    const n = Number(bytes || 0);
    if (n <= 0) return "";
    const mb = n / (1024 * 1024);
    return mb >= 1024 ? (mb / 1024).toFixed(1) + " GB" : mb.toFixed(1) + " MB";
}

function componentCard(c) {
    // 「装没装组件」与「能不能用」是两件事：整包用户、手动指定的用户都没装组件，
    // 但刮削/下载照常工作。卡片上把它们混成一句「未安装」会让人以为必须点下载。
    const usable = !!c.path;
    let st = COMPONENT_STATE[c.state] || { text: c.state || "未知", cls: "cancelled" };
    if (!usable && c.state === "missing") {
        st = { text: "缺失", cls: "failed" };
    } else if (usable && (c.state === "missing" || c.state === "outdated")) {
        st = { text: c.source ? c.source + "可用" : "可用", cls: "done" };
    }
    const parts = [];
    if (c.installedVersion) parts.push("已装 " + c.installedVersion);
    if (c.availableVersion && c.availableVersion !== c.installedVersion) {
        parts.push("可装 " + c.availableVersion);
    }
    if (c.size) parts.push(humanSize(c.size));
    const meta = parts.join(" · ") || "暂无版本信息";

    // 生效路径 + 来源：这是排查「装了却不生效」的唯一线索，必须显示。
    const origin = c.path
        ? `<div class="comp-path" title="${escapeHtml(c.path)}">${escapeHtml(c.source || "")}：${escapeHtml(c.path)}</div>`
        : "";

    let action = "";
    if (c.installing) {
        const pct = c.progressTotal ? Math.min(100, (c.progressDone / c.progressTotal) * 100) : 0;
        action = `<div class="bar small"><i style="width:${pct.toFixed(1)}%"></i></div>
                  <div class="comp-meta">下载中 ${humanSize(c.progressDone)}${
                      c.progressTotal ? " / " + humanSize(c.progressTotal) : ""
                  }</div>`;
    } else {
        const buttons = [];
        if (c.canInstall && c.onDemand && c.state !== "ready") {
            const label = c.state === "outdated" ? "更新" : "下载安装";
            buttons.push(`<button data-install="${escapeHtml(c.name)}">${label}${c.size ? "（" + humanSize(c.size) + "）" : ""}</button>`);
        }
        if (c.installedVersion) {
            buttons.push(`<button class="ghost" data-remove="${escapeHtml(c.name)}">移除</button>`);
        }
        // 有可用可执行文件时不再报「无法确定下载信息」：对这类用户，装组件只是
        // 「换一份更新的副本」，不是能用与否的问题。
        if (buttons.length === 0 && c.message && !usable) {
            buttons.push(`<span class="hint bad">${escapeHtml(c.message)}</span>`);
        }
        action = `<div class="comp-actions">${buttons.join("")}</div>`;
    }

    return `<div class="comp">
        <div class="comp-head">
            <strong>${escapeHtml(c.label || c.name)}</strong>
            <span class="tag ${st.cls}">${st.text}</span>
        </div>
        <div class="comp-meta">${escapeHtml(meta)}</div>
        ${origin}
        ${action}
    </div>`;
}

function renderComponents() {
    const box = el("componentList");
    if (box) {
        if (components.length === 0) {
            box.innerHTML = '<p class="hint">未读到组件信息（组件目录不可用，或本构建没有清单）</p>';
        } else {
            box.innerHTML = components.map(componentCard).join("");
            box.querySelectorAll("[data-install]").forEach((btn) => {
                btn.addEventListener("click", () => installComponent(btn.dataset.install));
            });
            box.querySelectorAll("[data-remove]").forEach((btn) => {
                btn.addEventListener("click", () => removeComponent(btn.dataset.remove));
            });
        }
    }
    if (el("componentsDir")) {
        el("componentsDir").textContent = componentsDir ? "安装目录：" + componentsDir : "";
    }
    renderComponentBanner();
}

// 任务页顶部的缺口提示：只在「真的缺、且用户没关掉入口」时出现。
function renderComponentBanner() {
    const banner = el("componentBanner");
    if (!banner) return;
    const missing = components.filter(
        (c) => !c.optional && !c.installing && !c.path && ["missing", "corrupt", "incompatible"].includes(c.state)
    );
    const onDemand = components.length === 0 ? true : components[0].onDemand !== false;
    if (missing.length === 0 || !onDemand) {
        banner.classList.add("hidden");
        return;
    }
    const names = missing.map((c) => c.label || c.name).join("、");
    const total = missing.reduce((sum, c) => sum + (c.size || 0), 0);
    // 拿不到清单时（断网且没有内置清单）体积未知，此时宁可不写，也别编一个数。
    const sizeText = total ? `（约 ${humanSize(total)}）` : "";
    el("componentBannerText").textContent = `还差一步：缺少${names}${sizeText}，装好才能开始。`;
    banner.classList.remove("hidden");
}

async function installComponent(name) {
    if (!api()) return;
    try {
        await api().InstallComponent(name);
        const c = components.find((x) => x.name === name);
        if (c) {
            c.installing = true;
            c.progressDone = 0;
            c.progressTotal = c.size || 0;
            renderComponents();
        }
    } catch (err) {
        appendLog({ level: "error", text: "启动下载失败: " + err });
    }
}

async function removeComponent(name) {
    if (!api()) return;
    try {
        await api().RemoveComponent(name);
    } catch (err) {
        appendLog({ level: "error", text: "移除失败: " + err });
    }
    await refreshComponents();
}

function gotoComponents() {
    const tab = document.querySelector('.tab[data-view="settings"]');
    if (tab) tab.click();
    const box = el("componentList");
    if (box) box.scrollIntoView({ block: "center" });
}

/* ---------- 设置页 ---------- */

function renderSettings(cfg) {
    el("saveDir").value = cfg.saveDir;
    el("txtPathInput").value = cfg.txtPath;
    el("dlrPath").value = cfg.dlrPath;
    el("pythonCmd").value = cfg.pythonCmd;
    if (el("ffmpegPath")) el("ffmpegPath").value = cfg.ffmpegPath;
    if (el("playerPath")) el("playerPath").value = cfg.playerPath;
    if (el("componentMirror")) el("componentMirror").value = cfg.componentMirror;
    if (el("componentsOnDemand")) el("componentsOnDemand").checked = cfg.componentsOnDemand !== false;

    el("threads").value = cfg.download.threads ?? 25;
    el("retryCount").value = cfg.download.retryCount ?? 20;
    el("timeoutSec").value = cfg.download.timeoutSec ?? 30;
    el("maxSpeed").value = cfg.download.maxSpeed ?? "";
    el("muxToMP4").checked = cfg.download.muxToMP4 !== false;
    el("scrapeTimeoutSec").value = cfg.scrapeTimeoutSec ?? 180;

    renderSites(cfg.sites);
    renderRecent(cfg.recentTxt);
    renderComponents();
}

function renderSites(sites) {
    const box = el("siteList");
    const names = Object.keys(sites).sort();
    box.innerHTML = names
        .map((name) => {
            const s = sites[name];
            return `<div class="site-row ${s.enabled ? "" : "off"}" data-site="${escapeHtml(name)}">
                <label class="site-name">
                    <input type="checkbox" data-role="enabled" ${s.enabled ? "checked" : ""} />
                    ${escapeHtml(name)}
                </label>
                <div class="kw">
                    <span>关键字</span>
                    <input type="text" data-role="keyword" value="${escapeHtml(s.keyword || "")}" />
                </div>
            </div>`;
        })
        .join("");

    box.querySelectorAll(".site-row").forEach((row) => {
        const name = row.dataset.site;
        const toggle = () => row.classList.toggle("off", !row.querySelector('[data-role="enabled"]').checked);
        row.querySelector('[data-role="enabled"]').addEventListener("change", toggle);
    });
}

function renderRecent(list) {
    const box = el("recentTxt");
    if (!list || list.length === 0) {
        box.innerHTML = "";
        return;
    }
    box.innerHTML = list
        .slice(0, 6)
        .map((p) => `<button data-recent="${escapeHtml(p)}">${escapeHtml(baseName(p))}</button>`)
        .join("");
    box.querySelectorAll("button[data-recent]").forEach((btn) => {
        btn.addEventListener("click", () => {
            el("txtPathInput").value = btn.dataset.recent;
        });
    });
}

function collectConfig() {
    const cfg = normalize(draft);
    cfg.saveDir = el("saveDir").value.trim();
    cfg.txtPath = el("txtPathInput").value.trim();
    cfg.dlrPath = el("dlrPath").value.trim();
    cfg.pythonCmd = el("pythonCmd").value.trim();
    if (el("ffmpegPath")) cfg.ffmpegPath = el("ffmpegPath").value.trim();
    if (el("playerPath")) cfg.playerPath = el("playerPath").value.trim();
    if (el("componentMirror")) cfg.componentMirror = el("componentMirror").value.trim();
    if (el("componentsOnDemand")) cfg.componentsOnDemand = el("componentsOnDemand").checked;
    cfg.scrapeTimeoutSec = intVal("scrapeTimeoutSec", 180);
    cfg.download = {
        threads: intVal("threads", 25),
        retryCount: intVal("retryCount", 20),
        timeoutSec: intVal("timeoutSec", 30),
        maxSpeed: el("maxSpeed").value.trim(),
        muxToMP4: el("muxToMP4").checked,
    };

    const sites = {};
    document.querySelectorAll(".site-row").forEach((row) => {
        sites[row.dataset.site] = {
            enabled: row.querySelector('[data-role="enabled"]').checked,
            keyword: row.querySelector('[data-role="keyword"]').value.trim(),
        };
    });
    cfg.sites = sites;
    return cfg;
}

function intVal(id, fallback) {
    const n = parseInt(el(id).value, 10);
    return Number.isFinite(n) && n > 0 ? n : fallback;
}

/* ---------- 按钮 ---------- */

function bindButtons() {
    el("btnPickTxt").addEventListener("click", async () => {
        const p = await api().SelectTxt();
        if (!p) return;
        el("txtPathInput").value = p;
        await saveSettings(true);
    });

    el("btnStart").addEventListener("click", async () => {
        el("saveHint").textContent = "";
        try {
            await api().Start();
        } catch (err) {
            appendLog({ level: "error", text: String(err) });
            el("issueBanner").textContent = String(err);
            el("issueBanner").classList.remove("hidden");
        }
    });

    el("btnStop").addEventListener("click", async () => {
        await api().Stop();
        appendLog({ level: "warn", text: "已请求停止" });
    });

    // 把成功的那些 URL 从清单里抹掉，失败的留下：下一次「开始」就只重试剩下的。
    // txt 里的行删掉后，界面上的任务队列也一并清空（后端一起做了）。
    el("btnPrune").addEventListener("click", async () => {
        try {
            const res = await api().ClearDownloaded();
            const file = baseName((res && res.path) || "txt");
            if (!res || !res.removed) {
                // 两种可能：本轮没有成功的条目，或清单里早就没有这些行了
                // （上次已清理 / 换了 txt）。话术上不猜是哪一种。
                appendLog({
                    level: "info",
                    text: `没有可删除的 URL：${file} 里找不到本轮已下载的条目（清单还剩 ${
                        res ? res.kept : 0
                    } 条），任务队列已清空`,
                });
                return;
            }
            appendLog({
                level: "info",
                text: `已从 ${file} 删除 ${res.removed} 条已下载 URL，清单剩余 ${
                    res.kept
                } 条 —— 任务队列已清空，下次「开始」只跑剩下的`,
            });
        } catch (err) {
            appendLog({ level: "error", text: "清除已下载 URL 失败: " + err });
        }
    });

    el("btnClearLog").addEventListener("click", () => {
        el("log").innerHTML = "";
        logCount = 0;
        followLog = true;
        logScroll = { top: 0, remainder: 0 };
    });

    document.querySelectorAll("[data-pick]").forEach((btn) => {
        btn.addEventListener("click", async () => {
            const kind = btn.dataset.pick;
            // 必须按类型分派：txt 要选文件，saveDir 选目录，dlr 选可执行文件
            const bind = api();
            const pickers = {
                txt: bind.SelectTxt,
                saveDir: bind.SelectDir,
                dlr: bind.SelectDlr,
                player: bind.SelectPlayer,
            };
            const fn = pickers[kind];
            if (!fn) return;
            const p = await fn();
            if (!p) return;
            if (kind === "saveDir") el("saveDir").value = p;
            if (kind === "txt") el("txtPathInput").value = p;
            if (kind === "dlr") el("dlrPath").value = p;
            if (kind === "player") el("playerPath").value = p;
        });
    });

    if (el("btnGoComponents")) {
        el("btnGoComponents").addEventListener("click", () => gotoComponents());
    }

    el("btnSave").addEventListener("click", () => saveSettings(false));
}

async function saveSettings(quiet) {
    if (!api()) return;
    const cfg = collectConfig();
    try {
        const info = await api().SaveConfig(cfg);
        draft = normalize(info.config);
        renderEnv(info);
        renderRecent(info.config.recentTxt);
        el("txtPath").textContent = info.config.txtPath || "未选择 txt 文件";
        el("txtPath").classList.toggle("set", !!info.config.txtPath);
        if (!quiet) {
            el("saveHint").textContent = "已保存 · " + new Date().toLocaleTimeString();
            el("saveHint").className = "hint";
        }
        if (info.issues && info.issues.length) {
            el("saveHint").textContent = "已保存，但仍有 " + info.issues.length + " 项待处理";
            el("saveHint").className = "hint bad";
        }
    } catch (err) {
        el("saveHint").textContent = "保存失败: " + err;
        el("saveHint").className = "hint bad";
    }
}

/* ---------- 工具 ---------- */

function baseName(p) {
    const i = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
    return i >= 0 ? p.slice(i + 1) : p;
}

function escapeHtml(s) {
    return String(s == null ? "" : s)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&quot;")
        .replace(/'/g, "&#39;");
}

document.addEventListener("DOMContentLoaded", boot);
