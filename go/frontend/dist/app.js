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

// 纯函数与 node:test 共用一份实现（见 pure.js）
const { clone, normalize, humanSize, baseName, dirName, escapeHtml, summarize } = window.JabPure;

let draft = null; // 设置页正在编辑的配置副本（输入 → draft 单向流动）
let savedConfig = null; // 最近一次保存成功的配置，用来判断「有没有未保存修改」
// 外部组件的最近一次状态。它同时被设置页的卡片与任务页的提示条使用，
// 所以放在模块级：两个视图渲染同源数据，不会出现「设置页说没装、任务页说装好了」。
let components = [];
let componentsDir = "";
let lastState = { running: false, index: 0, total: 0, items: [] };
let logCount = 0;
let followLog = true;
let logScroll = { top: 0, remainder: 0 };
const LOG_LIMIT = 2000;
let logFilter = { level: "all", query: "" };

/* ---------- 启动 ---------- */

async function boot() {
    bindTabs();
    bindButtons();
    bindTaskActions();
    bindPaneDivider();
    bindSettings();
    bindLogTools();
    bindModal();
    bindComponentActions();
    bindSettingsNav();

    if (!api()) {
        appendLog({ level: "error", text: "未检测到后端绑定，请通过 JabGrab 程序启动本界面。" });
        el("envBadge").textContent = "后端未连接";
        el("envBadge").className = "env bad";
        return;
    }

    try {
        const info = await api().Boot();
        savedConfig = normalize(info.config);
        draft = clone(savedConfig);
        componentsDir = info.componentsDir || "";
        setComponents(info.components || [], info);
        renderEnv(info);
        renderSettings(draft);
        markDirty();
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

    // 状态快照可能一秒来好几条（进度限流后仍有 ~7 条/秒），
    // 用 rAF 合帧：一帧内只渲染最后一份快照。
    window.runtime.EventsOn("pipeline:state", (s) => queueRender(s));
    window.runtime.EventsOn("pipeline:log", (l) => appendLog(l));

    // 组件事件：进度按 100ms 限流后推来，只更新进度条与文字，不重建卡片；
    // done/failed 是终态。刷新（refreshed）只表示「清单拉回来了」，重取一次最稳。
    window.runtime.EventsOn("component:progress", (p) => onComponentProgress(p));
    window.runtime.EventsOn("component:done", (p) => {
        appendLog({ level: "info", text: `组件 ${p.name} ${p.version} 已安装` });
        toast(`组件 ${p.name} 已安装`, "ok");
        refreshComponents();
    });
    window.runtime.EventsOn("component:failed", (p) => {
        appendLog({ level: "error", text: `组件 ${p.name} 安装失败：${p.message}` });
        toast(`组件 ${p.name} 安装失败：${p.message}`, "error");
        refreshComponents();
    });
    window.runtime.EventsOn("component:refreshed", () => refreshComponents());

    // 下载器版本是后台异步探测的，探测完成后补进徽标。
    window.runtime.EventsOn("dlr:version", (v) => {
        if (!v || !v.version) return;
        const badge = el("envBadge");
        if (badge.classList.contains("ok")) badge.textContent = v.version;
    });
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

function bindSettingsNav() {
    document.querySelectorAll("[data-goto]").forEach((btn) => {
        btn.addEventListener("click", () => {
            const card = el(btn.dataset.goto);
            if (!card) return;
            if (card.tagName === "DETAILS") card.open = true;
            card.scrollIntoView({ behavior: "smooth", block: "start" });
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

// 一帧内只渲染最后一份快照：进度事件再密，DOM 更新也不超过屏幕刷新率。
let pendingState = null;
let renderScheduled = false;

function queueRender(s) {
    if (!s) return;
    pendingState = s;
    if (renderScheduled) return;
    renderScheduled = true;
    const flush = () => {
        renderScheduled = false;
        const next = pendingState;
        pendingState = null;
        renderState(next);
    };
    // 页面隐藏（窗口最小化 / 后台标签页）时 rAF 不触发，退回定时器，
    // 否则状态会一直卡在最后一帧，恢复可见前 DOM 与后端对不上。
    if (document.hidden) setTimeout(flush, 16);
    else requestAnimationFrame(flush);
}

// 行级差量：URL 序列没变时只重建「内容签名」变了的行。
// 全量 innerHTML 重建会打断文本选择、让按钮闪断，几十行任务时还白白烧 CPU。
let rowUrls = [];
let rowSigs = [];

function rowSig(it, running) {
    return JSON.stringify([
        it.status, it.percent, it.speed, it.eta, it.downloaded, it.totalSize,
        it.name, it.message, it.path, it.actors, running,
    ]);
}

function renderState(s) {
    if (!s) return;
    lastState = s;

    const items = s.items || [];
    const body = el("taskBody");
    const running = !!s.running;

    if (items.length === 0) {
        body.innerHTML = `<tr class="empty"><td colspan="7">
            <div class="empty-box">
                <p>没有可处理的任务，请选择 txt 并确认已启用站点</p>
                <button class="primary" data-action="pick">选择 txt</button>
            </div>
        </td></tr>`;
        rowUrls = [];
        rowSigs = [];
        setOverallSegments(0, 0, 0);
        el("summaryText").textContent = "等待开始";
        el("summaryDetail").textContent = "";
        setRunning(running);
        return;
    }

    const urls = items.map((it) => it.url || "");
    const sameRows =
        urls.length === rowUrls.length && urls.every((u, i) => u === rowUrls[i]);
    if (!sameRows) {
        body.innerHTML = items.map((it, i) => rowHtml(it, i, running)).join("");
        rowSigs = items.map((it) => rowSig(it, running));
    } else {
        const rows = body.children;
        items.forEach((it, i) => {
            const sig = rowSig(it, running);
            if (sig === rowSigs[i]) return;
            rowSigs[i] = sig;
            rows[i].className = it.status === "failed" ? "row-failed" : "";
            rows[i].innerHTML = rowCells(it, i, running);
        });
    }
    rowUrls = urls;

    // 总体进度分三段：成功（绿）、失败/取消（红）、当前条目（蓝，按实际百分比）。
    const { ok, bad, cur, current } = summarize(items);
    const n = items.length;
    setOverallSegments((ok / n) * 100, (bad / n) * 100, (cur / n) * 100);

    setRunning(running);

    if (running) {
        el("summaryText").textContent = `第 ${s.index} / ${s.total} 条`;
        el("summaryDetail").textContent = current
            ? `${current.name || ""} · ${(current.percent || 0).toFixed(1)}%`
            : "处理中…";
    } else {
        el("summaryText").textContent = `共 ${s.total} 条，完成 ${ok}${bad ? `，失败/取消 ${bad}` : ""}`;
        el("summaryDetail").textContent = ok + bad === s.total && s.total > 0 ? "本轮结束" : "";
    }
}

function rowHtml(it, i, running) {
    const cls = it.status === "failed" ? ' class="row-failed"' : "";
    return `<tr${cls}>${rowCells(it, i, running)}</tr>`;
}

function rowCells(it, i, running) {
    const st = it.status || "pending";
    const pct = Math.min(100, Number(it.percent || 0));
    const msg = it.message || "";

    // 状态与进度合并成一列：下载中给进度条；失败永远显示原因（哪怕有过半截进度）；
    // 其余状态显示后端给的说明（已存在 / 取消原因等）。
    // 说明文字与状态标签一字不差时不再画第二遍：状态列里一蓝一灰两行「爬取中」
    // 看着像两个互相矛盾的状态（用户反馈的「爬取中会显示两个」）。
    const tagText = STATUS_TEXT[st] || st;
    let extra = "";
    if (st === "downloading") {
        extra = `<div class="bar small"><i style="width:${pct.toFixed(1)}%"></i></div>
            <div class="progress-meta">
                <span>${pct.toFixed(1)}%</span>
                <span>${it.downloaded ? escapeHtml(it.downloaded + " / " + (it.totalSize || "?")) : ""}</span>
            </div>`;
    } else if (st === "failed") {
        extra = `<div class="state-msg err" title="${escapeHtml(msg)}">${escapeHtml(msg || "未知错误")}</div>`;
    } else if (msg && msg !== tagText) {
        extra = `<div class="state-msg" title="${escapeHtml(msg)}">${escapeHtml(msg)}</div>`;
    }

    const speed = it.speed ? `${it.speed}${it.eta ? " · " + it.eta : ""}` : "";

    const actors = Array.isArray(it.actors) ? it.actors.filter(Boolean) : [];
    const actorText = actors.join("、");
    const actorCell = actorText
        ? `<span class="actor" title="${escapeHtml(actorText)}">${escapeHtml(actorText)}</span>`
        : '<span class="hint">—</span>';

    // 操作合并成一列：成品存在才有播放/文件夹；复制永远可用；
    // 重试只给失败/取消的行，移除只在停着的时候给（运行中动队列会和后端对不上）。
    const acts = [];
    if (it.path) {
        acts.push(`<button class="mini play-btn" data-action="play" data-path="${escapeHtml(it.path)}"
            title="播放 ${escapeHtml(it.path)}">播放</button>`);
        acts.push(`<button class="mini reveal-btn" data-action="reveal" data-path="${escapeHtml(it.path)}"
            title="打开 ${escapeHtml(dirName(it.path))}">文件夹</button>`);
    }
    acts.push(`<button class="mini ghost" data-action="copy" data-url="${escapeHtml(it.url || "")}"
        title="复制这条 URL">复制</button>`);
    if (!running && (st === "failed" || st === "cancelled")) {
        acts.push(`<button class="mini" data-action="retry" data-idx="${i}" title="只重试这一条">重试</button>`);
    }
    if (!running) {
        acts.push(`<button class="mini ghost danger-text" data-action="remove" data-idx="${i}"
            title="从队列移除（不改 txt 文件）">移除</button>`);
    }

    return `
        <td class="col-idx">${i + 1}</td>
        <td class="cell-name">
            <div class="name">${escapeHtml(it.name || "—")}</div>
            <div class="url" title="${escapeHtml(it.url || "")}">${escapeHtml(it.url || "")}</div>
        </td>
        <td class="col-actor cell-actor">${actorCell}</td>
        <td class="col-site"><span class="tag">${escapeHtml(it.site || "-")}</span></td>
        <td class="col-state">
            <div class="state-cell">
                <span class="tag ${escapeHtml(st)}">${escapeHtml(tagText)}</span>
                ${extra}
            </div>
        </td>
        <td class="col-speed speed-cell">${escapeHtml(speed)}</td>
        <td class="col-actions"><div class="cell-actions">${acts.join("")}</div></td>`;
}

function setOverallSegments(okPct, badPct, curPct) {
    const clamp = (v) => Math.max(0, Math.min(100, v)).toFixed(2) + "%";
    el("segOk").style.width = clamp(okPct);
    el("segBad").style.width = clamp(badPct);
    el("segCur").style.width = clamp(curPct);
}

function setRunning(running) {
    el("btnStart").disabled = running;
    el("btnStop").disabled = !running;
    el("btnPickTxt").disabled = running;
    // 运行中不能改 txt：这一轮还在往里写状态，删完列表与文件必然对不上。
    el("btnPrune").disabled = running;
}

/* ---------- 行内操作（事件委托，绑一次） ---------- */

function bindTaskActions() {
    el("taskBody").addEventListener("click", (event) => {
        const btn = event.target.closest("[data-action]");
        if (!btn) return;
        switch (btn.dataset.action) {
            case "pick":
                el("btnPickTxt").click();
                break;
            case "play":
                playFile(btn.dataset.path);
                break;
            case "reveal":
                revealFile(btn.dataset.path);
                break;
            case "copy":
                copyText(btn.dataset.url);
                break;
            case "retry":
                retryTask(Number(btn.dataset.idx));
                break;
            case "remove":
                removeTask(Number(btn.dataset.idx));
                break;
        }
    });
}

async function playFile(path) {
    if (!api()) return;
    try {
        await api().PlayFile(path);
        appendLog({ level: "info", text: `已交给播放器：${baseName(path)}` });
    } catch (err) {
        appendLog({ level: "error", text: "播放失败: " + err });
        toast("播放失败: " + err, "error");
    }
}

async function revealFile(path) {
    if (!api()) return;
    try {
        await api().RevealFile(path);
        appendLog({ level: "info", text: `已打开所在目录：${dirName(path)}` });
    } catch (err) {
        appendLog({ level: "error", text: "打开文件夹失败: " + err });
        toast("打开文件夹失败: " + err, "error");
    }
}

async function copyText(text) {
    if (!text) return;
    try {
        await navigator.clipboard.writeText(text);
        toast("链接已复制", "ok");
    } catch {
        // 个别 WebView 没开剪贴板权限，退回老办法
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        try {
            document.execCommand("copy");
            toast("链接已复制", "ok");
        } catch {
            toast("复制失败，请手动选择 URL", "error");
        }
        ta.remove();
    }
}

async function retryTask(idx) {
    if (!api()) return;
    try {
        await api().RetryTask(idx);
    } catch (err) {
        appendLog({ level: "error", text: "重试失败: " + err });
        toast(String(err), "error");
    }
}

async function removeTask(idx) {
    if (!api()) return;
    const it = (lastState.items || [])[idx];
    try {
        await api().RemoveTask(idx);
        appendLog({ level: "info", text: `已从队列移除：${(it && it.url) || idx + 1}` });
    } catch (err) {
        appendLog({ level: "error", text: "移除失败: " + err });
        toast(String(err), "error");
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
    const level = line.level || "info";
    div.className = "l-" + level;
    div.dataset.level = level;
    div.textContent = line.text || "";
    if (!lineVisible(div)) div.classList.add("filtered");
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

function lineVisible(div) {
    const lv = div.dataset.level || "info";
    if (logFilter.level !== "all") {
        const ok =
            logFilter.level === "info"
                ? lv === "info" || lv === "debug"
                : lv === logFilter.level;
        if (!ok) return false;
    }
    if (logFilter.query && !div.textContent.toLowerCase().includes(logFilter.query)) {
        return false;
    }
    return true;
}

function applyLogFilter() {
    const pre = el("log");
    for (const div of pre.children) {
        div.classList.toggle("filtered", !lineVisible(div));
    }
    if (followLog) pre.scrollTop = pre.scrollHeight;
}

function bindLogTools() {
    el("logFilters").addEventListener("click", (event) => {
        const btn = event.target.closest("[data-level]");
        if (!btn) return;
        logFilter.level = btn.dataset.level;
        el("logFilters").querySelectorAll(".chip").forEach((c) => {
            c.classList.toggle("active", c === btn);
        });
        applyLogFilter();
    });
    let timer = 0;
    el("logSearch").addEventListener("input", () => {
        clearTimeout(timer);
        timer = setTimeout(() => {
            logFilter.query = el("logSearch").value.trim().toLowerCase();
            applyLogFilter();
        }, 150);
    });
}

/* ---------- 轻提示与确认弹窗 ---------- */

function toast(text, kind = "info") {
    const box = el("toasts");
    const div = document.createElement("div");
    div.className = "toast " + kind;
    div.textContent = text;
    div.title = "点击关闭";
    div.addEventListener("click", () => div.remove());
    box.appendChild(div);
    while (box.children.length > 4) box.firstChild.remove();
    setTimeout(() => {
        div.classList.add("out");
        setTimeout(() => div.remove(), 260);
    }, 3500);
}

let modalResolve = null;

function confirmDialog({ title, text, okText = "确定", danger = false }) {
    return new Promise((resolve) => {
        el("modalTitle").textContent = title;
        el("modalText").textContent = text;
        const ok = el("btnModalOk");
        ok.textContent = okText;
        ok.className = danger ? "danger" : "primary";
        el("modalMask").classList.remove("hidden");
        modalResolve = resolve;
        ok.focus();
    });
}

function closeModal(result) {
    el("modalMask").classList.add("hidden");
    if (modalResolve) {
        modalResolve(result);
        modalResolve = null;
    }
}

function bindModal() {
    el("btnModalOk").addEventListener("click", () => closeModal(true));
    el("btnModalCancel").addEventListener("click", () => closeModal(false));
    el("modalMask").addEventListener("click", (event) => {
        if (event.target === el("modalMask")) closeModal(false);
    });
    document.addEventListener("keydown", (event) => {
        if (event.key === "Escape" && !el("modalMask").classList.contains("hidden")) {
            closeModal(false);
        }
    });
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

function componentCard(c) {
    // 「装没装组件」与「能不能用」是两件事：整包用户、手动指定的用户都没装组件，
    // 但刮削/下载照常工作。卡片上把它们混成一句「未安装」会让人以为必须点下载。
    const usable = !!c.path;
    let st = COMPONENT_STATE[c.state] || { text: escapeHtml(c.state || "未知"), cls: "cancelled" };
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
        action = `<div class="bar small"><i data-bar style="width:${pct.toFixed(1)}%"></i></div>
                  <div class="comp-actions">
                      <span class="comp-meta" data-meta>下载中 ${humanSize(c.progressDone)}${
                          c.progressTotal ? " / " + humanSize(c.progressTotal) : ""
                      }</span>
                      <button class="ghost" data-cancel="${escapeHtml(c.name)}">取消</button>
                  </div>`;
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

    return `<div class="comp" data-comp="${escapeHtml(c.name)}">
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
        box.innerHTML =
            components.length === 0
                ? '<p class="hint">未读到组件信息（组件目录不可用，或本构建没有清单）</p>'
                : components.map(componentCard).join("");
    }
    if (el("componentsDir")) {
        el("componentsDir").textContent = componentsDir ? "安装目录：" + componentsDir : "";
    }
    renderComponentBanner();
}

// 进度事件每秒来十条：只改进度条宽度与字节数文字，不重建卡片。
// 重建会打断「取消」按钮的点击，还让整页跟着闪。
function onComponentProgress(p) {
    const c = components.find((x) => x.name === p.name);
    if (!c) return;
    const wasInstalling = c.installing;
    c.installing = true;
    c.progressDone = p.done;
    c.progressTotal = p.total;
    const card = document.querySelector(`[data-comp="${CSS.escape(p.name)}"]`);
    if (!wasInstalling || !card) {
        renderComponents();
        return;
    }
    const bar = card.querySelector("[data-bar]");
    const meta = card.querySelector("[data-meta]");
    const pct = p.total ? Math.min(100, (p.done / p.total) * 100) : 0;
    if (bar) bar.style.width = pct.toFixed(1) + "%";
    if (meta) {
        meta.textContent = `下载中 ${humanSize(p.done)}${p.total ? " / " + humanSize(p.total) : ""}`;
    }
}

function bindComponentActions() {
    const box = el("componentList");
    if (!box) return;
    box.addEventListener("click", (event) => {
        const install = event.target.closest("[data-install]");
        if (install) return installComponent(install.dataset.install);
        const remove = event.target.closest("[data-remove]");
        if (remove) return removeComponent(remove.dataset.remove);
        const cancel = event.target.closest("[data-cancel]");
        if (cancel) return cancelInstall(cancel.dataset.cancel);
    });
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
        toast("启动下载失败: " + err, "error");
    }
}

async function cancelInstall(name) {
    if (!api()) return;
    try {
        await api().CancelInstall(name);
        appendLog({ level: "warn", text: `已请求取消组件 ${name} 的下载` });
    } catch (err) {
        appendLog({ level: "error", text: "取消下载失败: " + err });
    }
}

async function removeComponent(name) {
    if (!api()) return;
    const ok = await confirmDialog({
        title: "移除组件",
        text: `确定移除组件 ${name} 的已安装副本？之后可随时重新下载。`,
        okText: "移除",
        danger: true,
    });
    if (!ok) return;
    try {
        await api().RemoveComponent(name);
    } catch (err) {
        appendLog({ level: "error", text: "移除失败: " + err });
        toast("移除失败: " + err, "error");
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
}

// 输入 → draft 单向流动：DOM 事件只改 draft，保存时把 draft 交给后端；
// 后端回包再整体写回 DOM。不再在保存时从 DOM 反读，避免「界面一套、配置一套」。
function setDraftField(id, value) {
    if (!draft) return;
    switch (id) {
        case "saveDir": draft.saveDir = value; break;
        case "txtPathInput": draft.txtPath = value; break;
        case "dlrPath": draft.dlrPath = value; break;
        case "pythonCmd": draft.pythonCmd = value; break;
        case "ffmpegPath": draft.ffmpegPath = value; break;
        case "playerPath": draft.playerPath = value; break;
        case "componentMirror": draft.componentMirror = value; break;
        case "componentsOnDemand": draft.componentsOnDemand = value; break;
        case "threads": draft.download.threads = value; break;
        case "retryCount": draft.download.retryCount = value; break;
        case "timeoutSec": draft.download.timeoutSec = value; break;
        case "maxSpeed": draft.download.maxSpeed = value; break;
        case "muxToMP4": draft.download.muxToMP4 = value; break;
        case "scrapeTimeoutSec": draft.scrapeTimeoutSec = value; break;
        default: return;
    }
    markDirty();
}

function markDirty() {
    if (!draft || !savedConfig) return;
    const dirty = JSON.stringify(draft) !== JSON.stringify(savedConfig);
    el("saveBar").classList.toggle("dirty", dirty);
    el("btnReset").classList.toggle("hidden", !dirty);
    el("btnSave").disabled = !dirty;
    el("saveHint").textContent = dirty ? "有未保存的修改" : "";
    el("saveHint").className = "hint" + (dirty ? " warn-text" : "");
}

function bindSettings() {
    const view = el("view-settings");

    view.addEventListener("input", (event) => {
        const t = event.target;
        if (!draft) return;
        if (t.dataset.role === "keyword") {
            const row = t.closest(".site-row");
            if (row && draft.sites[row.dataset.site]) {
                draft.sites[row.dataset.site].keyword = t.value;
                markDirty();
            }
            return;
        }
        // 数字与复选框在 change 里处理（数字要夹取范围后写回）
        if (t.type === "number" || t.type === "checkbox") return;
        if (t.id) setDraftField(t.id, t.value);
    });

    view.addEventListener("change", (event) => {
        const t = event.target;
        if (!draft) return;
        if (t.dataset.role === "enabled") {
            const row = t.closest(".site-row");
            if (row && draft.sites[row.dataset.site]) {
                draft.sites[row.dataset.site].enabled = t.checked;
                row.classList.toggle("off", !t.checked);
                markDirty();
            }
            return;
        }
        if (t.type === "checkbox") {
            setDraftField(t.id, t.checked);
            return;
        }
        if (t.type === "number") {
            // 越界/乱填的值夹回范围并写回输入框，而不是悄悄换成默认值。
            const min = Number(t.min);
            const max = Number(t.max);
            let n = parseInt(t.value, 10);
            if (!Number.isFinite(n)) n = Number(t.dataset.fallback || t.min || 1);
            if (Number.isFinite(min)) n = Math.max(min, n);
            if (Number.isFinite(max)) n = Math.min(max, n);
            t.value = String(n);
            setDraftField(t.id, n);
            return;
        }
        if (t.id) setDraftField(t.id, t.value.trim());
    });

    el("recentTxt").addEventListener("click", (event) => {
        const btn = event.target.closest("[data-recent]");
        if (!btn) return;
        el("txtPathInput").value = btn.dataset.recent;
        setDraftField("txtPathInput", btn.dataset.recent);
    });

    el("btnReset").addEventListener("click", () => {
        draft = clone(savedConfig);
        renderSettings(draft);
        markDirty();
    });
}

/* ---------- 按钮 ---------- */

function bindButtons() {
    el("btnPickTxt").addEventListener("click", async () => {
        const p = await api().SelectTxt();
        if (!p) return;
        el("txtPathInput").value = p;
        setDraftField("txtPathInput", p);
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
            toast(String(err), "error");
        }
    });

    el("btnStop").addEventListener("click", async () => {
        await api().Stop();
        appendLog({ level: "warn", text: "已请求停止" });
    });

    // 把成功的那些 URL 从清单里抹掉，失败的留下：下一次「开始」就只重试剩下的。
    // 这是改用户文件的动作，先确认再动手；后端会先把原文件备份到配置目录。
    el("btnPrune").addEventListener("click", async () => {
        const items = lastState.items || [];
        const doneCount = items.filter((i) => ["done", "skipped"].includes(i.status)).length;
        const file = baseName((draft && draft.txtPath) || "txt");
        const ok = await confirmDialog({
            title: "清除已下载",
            text:
                `将从 ${file} 里删除已下载成功（完成 / 已存在）的 URL${doneCount ? `，本轮共 ${doneCount} 条` : ""}，` +
                "失败的保留等下次重试。\n原文件会先备份到配置目录（txt.bak），任务队列也会一并清空。",
            okText: "清除",
            danger: true,
        });
        if (!ok) return;
        try {
            const res = await api().ClearDownloaded();
            const name = baseName((res && res.path) || "txt");
            if (!res || !res.removed) {
                // 两种可能：本轮没有成功的条目，或清单里早就没有这些行了
                // （上次已清理 / 换了 txt）。话术上不猜是哪一种。
                appendLog({
                    level: "info",
                    text: `没有可删除的 URL：${name} 里找不到本轮已下载的条目（清单还剩 ${
                        res ? res.kept : 0
                    } 条），任务队列已清空`,
                });
                toast("没有可删除的 URL，任务队列已清空");
                return;
            }
            appendLog({
                level: "info",
                text: `已从 ${name} 删除 ${res.removed} 条已下载 URL，清单剩余 ${
                    res.kept
                } 条 —— 任务队列已清空，下次「开始」只跑剩下的`,
            });
            toast(`已删除 ${res.removed} 条已下载 URL`, "ok");
        } catch (err) {
            appendLog({ level: "error", text: "清除已下载 URL 失败: " + err });
            toast("清除已下载 URL 失败: " + err, "error");
        }
    });

    // 打开保存目录：下载途中也能点，直接看文件落到了哪儿。
    el("btnOpenDir").addEventListener("click", async () => {
        if (!api()) return;
        try {
            await api().OpenSaveDir();
        } catch (err) {
            appendLog({ level: "error", text: "打开文件夹失败: " + err });
            toast("打开文件夹失败: " + err, "error");
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
            const targets = { saveDir: "saveDir", txt: "txtPathInput", dlr: "dlrPath", player: "playerPath" };
            const id = targets[kind];
            if (id) {
                el(id).value = p;
                setDraftField(id, p);
            }
        });
    });

    if (el("btnGoComponents")) {
        el("btnGoComponents").addEventListener("click", () => gotoComponents());
    }

    el("btnSave").addEventListener("click", () => saveSettings(false));

    // 徽标有问题时点一下，直接定位到问题横幅
    el("envBadge").addEventListener("click", () => {
        const banner = el("issueBanner");
        if (banner.classList.contains("hidden")) return;
        banner.scrollIntoView({ block: "nearest" });
        banner.classList.remove("flash");
        void banner.offsetWidth;
        banner.classList.add("flash");
    });
}

async function saveSettings(quiet) {
    if (!api() || !draft) return;
    const cfg = clone(draft);
    for (const k of ["saveDir", "txtPath", "dlrPath", "pythonCmd", "ffmpegPath", "playerPath", "componentMirror"]) {
        cfg[k] = (cfg[k] || "").trim();
    }
    cfg.download.maxSpeed = (cfg.download.maxSpeed || "").trim();
    for (const s of Object.values(cfg.sites)) {
        s.keyword = (s.keyword || "").trim();
    }
    try {
        const info = await api().SaveConfig(cfg);
        savedConfig = normalize(info.config);
        draft = clone(savedConfig);
        renderEnv(info);
        renderSettings(draft);
        markDirty();
        if (!quiet) {
            toast("设置已保存", "ok");
            el("saveHint").textContent = "已保存 · " + new Date().toLocaleTimeString();
        }
        if (info.issues && info.issues.length) {
            el("saveHint").textContent = "已保存，但仍有 " + info.issues.length + " 项待处理";
            el("saveHint").className = "hint bad";
        }
    } catch (err) {
        el("saveHint").textContent = "保存失败: " + err;
        el("saveHint").className = "hint bad";
        toast("保存失败: " + err, "error");
    }
}

document.addEventListener("DOMContentLoaded", boot);