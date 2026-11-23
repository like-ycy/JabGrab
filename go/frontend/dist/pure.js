/* 纯函数：不碰 DOM 与后端绑定，app.js 与 node:test 共用同一份实现。
   浏览器里挂到 globalThis.JabPure，Node 里走 module.exports。 */
(function (root, factory) {
    const api = factory();
    if (typeof module !== "undefined" && module.exports) module.exports = api;
    root.JabPure = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
    "use strict";

    const clone = (o) =>
        typeof structuredClone === "function" ? structuredClone(o) : JSON.parse(JSON.stringify(o));

    function normalize(cfg) {
        const c = clone(cfg || {});
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

    function humanSize(bytes) {
        const n = Number(bytes || 0);
        if (n <= 0) return "";
        const mb = n / (1024 * 1024);
        return mb >= 1024 ? (mb / 1024).toFixed(1) + " GB" : mb.toFixed(1) + " MB";
    }

    function baseName(p) {
        const i = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
        return i >= 0 ? p.slice(i + 1) : p;
    }

    function dirName(p) {
        const i = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
        return i > 0 ? p.slice(0, i) : p;
    }

    function escapeHtml(s) {
        return String(s == null ? "" : s)
            .replace(/&/g, "&amp;")
            .replace(/</g, "&lt;")
            .replace(/>/g, "&gt;")
            .replace(/"/g, "&quot;")
            .replace(/'/g, "&#39;");
    }

    // 总体进度的三段汇总：成功（done/skipped）、失败/取消、当前下载条目的 fractional 进度。
    function summarize(items) {
        let ok = 0;
        let bad = 0;
        let cur = 0;
        let current = null;
        for (const it of items || []) {
            const st = it.status || "pending";
            if (st === "done" || st === "skipped") {
                ok += 1;
            } else if (st === "failed" || st === "cancelled") {
                bad += 1;
            } else if (st === "downloading") {
                cur += Math.min(1, (it.percent || 0) / 100);
                current = it;
            }
        }
        return { ok, bad, cur, current };
    }

    return { clone, normalize, humanSize, baseName, dirName, escapeHtml, summarize };
});