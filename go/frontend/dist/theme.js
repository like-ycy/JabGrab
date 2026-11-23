/* JabGrab 主题：亮色 / 暗色 / 跟随系统。
   与 video_cut 同一套行为：
   - 偏好存 localStorage，图标按偏好显示（太阳 / 月亮 / 显示器），点一次循环切换；
   - 「跟随系统」优先问原生外观（macOS 的 WKWebView 会误报 prefers-color-scheme），
     拿不到再退回 media query，并轮询纠正系统外观变化；
   - 切换时同步原生窗口底色，避免拖动/缩放时闪出白边。

   整体包在 IIFE 里：本页是普通 script，直接用全局作用域会和 app.js 的 const 撞名。 */
(function () {
    "use strict";

    const STORAGE_KEY = "jabgrab-theme";

    /** 与 style.css 中 --color-background 一致，供原生窗口底色使用。 */
    const NATIVE_BG = {
        light: [247, 248, 250],
        dark: [11, 13, 16],
    };

    const LABELS = {
        light: "亮色模式",
        dark: "暗色模式",
        system: "跟随系统",
    };

    /** lucide 图标（与 video_cut 的 Sun / Moon / Monitor 同一条路径）。 */
    const ICONS = {
        light:
            '<circle cx="12" cy="12" r="4"/>' +
            '<path d="M12 2v2"/><path d="M12 20v2"/>' +
            '<path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/>' +
            '<path d="M2 12h2"/><path d="M20 12h2"/>' +
            '<path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
        dark: '<path d="M20.985 12.486a9 9 0 1 1-9.473-9.472c.405-.022.617.46.402.803a6 6 0 0 0 8.268 8.268c.344-.215.825-.004.803.401"/>',
        system:
            '<rect width="20" height="14" x="2" y="3" rx="2"/>' +
            '<line x1="8" x2="16" y1="21" y2="21"/>' +
            '<line x1="12" x2="12" y1="17" y2="21"/>',
    };

    const app = () => window.go?.main?.App;

    let preference = "system";
    let resolved = "light";
    let pollTimer = 0;
    let mqList = null;
    let mqHandler = null;
    let warnedNative = false;

    const darkQuery = () =>
        typeof window.matchMedia === "function"
            ? window.matchMedia("(prefers-color-scheme: dark)")
            : null;

    function getThemePreference() {
        try {
            const raw = localStorage.getItem(STORAGE_KEY);
            if (raw === "light" || raw === "dark" || raw === "system") return raw;
        } catch {
            /* 隐私模式等场景下忽略 */
        }
        return "system";
    }

    function saveThemePreference(pref) {
        try {
            localStorage.setItem(STORAGE_KEY, pref);
        } catch {
            /* 同上 */
        }
    }

    /** 同步解析：显式 light/dark 直接返回；system 仅能用 media query 猜。 */
    function resolveThemeSync(pref) {
        if (pref === "light" || pref === "dark") return pref;
        return darkQuery()?.matches ? "dark" : "light";
    }

    /** 解析系统外观：优先原生侧，拿不到再回退 media query。 */
    async function resolveSystemTheme() {
        const bind = app();
        if (bind?.GetSystemAppearance) {
            try {
                const native = await bind.GetSystemAppearance();
                if (native === "dark" || native === "light") return native;
            } catch (err) {
                // 原生侧偶尔取不到（调试环境等）：退回 media query，但别静默降级。
                if (!warnedNative) {
                    warnedNative = true;
                    console.warn("读取系统外观失败，回退 prefers-color-scheme：", err);
                }
            }
        }
        return resolveThemeSync("system");
    }

    function applyTheme(next) {
        resolved = next;
        const root = document.documentElement;
        root.classList.toggle("dark", next === "dark");
        root.style.colorScheme = next;

        try {
            const [r, g, b] = NATIVE_BG[next];
            if (window.runtime?.WindowSetBackgroundColour) {
                window.runtime.WindowSetBackgroundColour(r, g, b, 1);
            }
        } catch {
            /* 浏览器调试环境没有 runtime，忽略 */
        }

        let meta = document.querySelector('meta[name="color-scheme"]');
        if (!meta) {
            meta = document.createElement("meta");
            meta.name = "color-scheme";
            document.head.appendChild(meta);
        }
        meta.content = next;

        renderButton();
    }

    function cycleTheme() {
        if (preference === "light") return "dark";
        if (preference === "dark") return "system";
        return "light";
    }

    function themeLabel() {
        return preference === "system"
            ? `跟随系统（当前${LABELS[resolved]}）`
            : LABELS[preference];
    }

    function renderButton() {
        const btn = document.getElementById("btnTheme");
        if (!btn) return;
        const label = themeLabel();
        btn.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
            stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[preference]}</svg>`;
        btn.title = `${label}（点击切换）`;
        btn.setAttribute("aria-label", label);
    }

    /** 系统外观可能在程序运行期间变化：轮询 + media query 双保险。 */
    function watchSystemTheme() {
        stopWatching();

        const sync = async () => {
            const next = await resolveSystemTheme();
            if (preference !== "system") return;
            if (next !== resolved) applyTheme(next);
        };

        void sync();
        pollTimer = window.setInterval(() => void sync(), 1500);

        const mq = darkQuery();
        if (mq) {
            mqList = mq;
            mqHandler = () => void sync();
            mq.addEventListener("change", mqHandler);
        }
    }

    function stopWatching() {
        if (pollTimer) {
            window.clearInterval(pollTimer);
            pollTimer = 0;
        }
        if (mqList && mqHandler) {
            mqList.removeEventListener("change", mqHandler);
        }
        mqList = null;
        mqHandler = null;
    }

    function setTheme(next) {
        if (next !== "light" && next !== "dark" && next !== "system") return;
        preference = next;
        saveThemePreference(next);

        if (next === "system") {
            // 先用同步猜测立刻出画面，再等原生结果纠正
            applyTheme(resolveThemeSync("system"));
            watchSystemTheme();
            return;
        }

        stopWatching();
        applyTheme(next);
    }

    function initTheme() {
        preference = getThemePreference();

        // 首帧已经由 index.html 里的内联脚本套用，这里只做纠正与绑定。
        if (preference === "system") {
            applyTheme(resolveThemeSync("system"));
            watchSystemTheme();
        } else {
            applyTheme(preference);
        }

        const btn = document.getElementById("btnTheme");
        if (btn) {
            btn.addEventListener("click", () => setTheme(cycleTheme()));
        }

        // 其它窗口改了偏好时同步过来
        window.addEventListener("storage", (event) => {
            if (event.key === STORAGE_KEY) setTheme(getThemePreference());
        });
    }

    window.JabTheme = {
        setTheme,
        get preference() {
            return preference;
        },
        get resolved() {
            return resolved;
        },
    };

    document.addEventListener("DOMContentLoaded", initTheme);
})();
