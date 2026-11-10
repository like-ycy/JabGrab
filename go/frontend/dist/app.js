/* JabGrab 前端：vanilla JS，无框架。
   后端方法通过 Wails 注入的 window.go.main.App 调用。 */

const api = () => window.go?.main?.App;

const STATUS_TEXT = {
    pending: "待处理",
    scraping: "爬取中",
    downloading: "下载中",
    done: "完成",
    skipped: "已跳过",
    ignored: "已忽略",
    failed: "失败",
    cancelled: "已取消",
};

const el = (id) => document.getElementById(id);

let draft = null; // 设置页正在编辑的配置副本
let lastState = { running: false, index: 0, total: 0, items: [] };
let logCount = 0;
const LOG_LIMIT = 2000;

/* ---------- 启动 ---------- */

async function boot() {
    bindTabs();
    bindButtons();

    if (!api()) {
        appendLog({ level: "error", text: "未检测到后端绑定，请通过 JabGrab 程序启动本界面。" });
        el("envBadge").textContent = "后端未连接";
        el("envBadge").className = "env bad";
        return;
    }

    try {
        const info = await api().Boot();
        draft = normalize(info.config);
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
}

function normalize(cfg) {
    const c = JSON.parse(JSON.stringify(cfg || {}));
    c.saveDir = c.saveDir || "";
    c.txtPath = c.txtPath || "";
    c.recentTxt = c.recentTxt || [];
    c.dlrPath = c.dlrPath || "";
    c.pythonCmd = c.pythonCmd || "";
    c.sites = c.sites || {};
    c.download = c.download || {};
    c.scrapeTimeoutSec = c.scrapeTimeoutSec || 180;
    return c;
}

/* ---------- 视图切换 ---------- */

function bindTabs() {
    document.querySelectorAll(".tab").forEach((btn) => {
        btn.addEventListener("click", () => {
            document.querySelectorAll(".tab").forEach((b) => b.classList.remove("active"));
            document.querySelectorAll(".view").forEach((v) => v.classList.remove("active"));
            btn.classList.add("active");
            el("view-" + btn.dataset.view).classList.add("active");
        });
    });
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
        : "实际执行：" + (info.scrapeCmd || []).join(" ");
    el("scrapeHint").className = "hint" + (info.scrapeErr ? " bad" : "");
}

/* ---------- 任务状态渲染 ---------- */

function renderState(s) {
    if (!s) return;
    lastState = s;

    const items = s.items || [];
    const body = el("taskBody");

    if (items.length === 0) {
        body.innerHTML = '<tr class="empty"><td colspan="6">还没有任务，选择 txt 后点“开始”</td></tr>';
        setOverall(0);
        el("summaryText").textContent = "等待开始";
        el("summaryDetail").textContent = "";
        setRunning(false);
        return;
    }

    const html = items.map((it, i) => {
        const st = it.status || "pending";
        const done = ["done", "skipped", "ignored", "failed", "cancelled"].includes(st);
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

        return `<tr class="${st === "failed" ? "row-failed" : ""}">
            <td class="col-idx">${i + 1}</td>
            <td class="cell-name">
                <div class="name">${escapeHtml(name || "—")}</div>
                <div class="url">${escapeHtml(it.url || "")}</div>
            </td>
            <td class="col-site"><span class="tag">${escapeHtml(it.site || "-")}</span></td>
            <td class="col-status"><span class="tag ${st}">${STATUS_TEXT[st] || st}</span></td>
            <td class="col-progress">${progress}</td>
            <td class="col-speed speed-cell">${escapeHtml(speed)}</td>
        </tr>`;
    });

    body.innerHTML = html.join("");

    // 总体进度：已结束的按 100%，当前下载中按实际百分比
    let acc = 0;
    let finished = 0;
    let current = null;
    for (const it of items) {
        const st = it.status || "pending";
        if (["done", "skipped", "ignored"].includes(st)) {
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
}

/* ---------- 日志 ---------- */

function appendLog(line) {
    const pre = el("log");
    const div = document.createElement("div");
    div.className = "l-" + (line.level || "info");
    div.textContent = line.text || "";
    pre.appendChild(div);
    logCount++;

    while (logCount > LOG_LIMIT && pre.firstChild) {
        pre.removeChild(pre.firstChild);
        logCount--;
    }
    pre.scrollTop = pre.scrollHeight;
}

/* ---------- 设置页 ---------- */

function renderSettings(cfg) {
    el("saveDir").value = cfg.saveDir;
    el("txtPathInput").value = cfg.txtPath;
    el("dlrPath").value = cfg.dlrPath;
    el("pythonCmd").value = cfg.pythonCmd;

    el("threads").value = cfg.download.threads ?? 25;
    el("retryCount").value = cfg.download.retryCount ?? 20;
    el("timeoutSec").value = cfg.download.timeoutSec ?? 30;
    el("maxSpeed").value = cfg.download.maxSpeed ?? "";
    el("muxToMP4").checked = cfg.download.muxToMP4 !== false;
    el("scrapeTimeoutSec").value = cfg.scrapeTimeoutSec ?? 180;

    renderSites(cfg.sites);
    renderRecent(cfg.recentTxt);
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

    el("btnClearLog").addEventListener("click", () => {
        el("log").innerHTML = "";
        logCount = 0;
    });

    document.querySelectorAll("[data-pick]").forEach((btn) => {
        btn.addEventListener("click", async () => {
            const kind = btn.dataset.pick;
            const fn = kind === "dlr" ? api().SelectDlr : api().SelectDir;
            const p = await fn();
            if (!p) return;
            if (kind === "saveDir") el("saveDir").value = p;
            if (kind === "txt") el("txtPathInput").value = p;
            if (kind === "dlr") el("dlrPath").value = p;
        });
    });

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
