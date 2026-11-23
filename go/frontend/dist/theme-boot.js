/* 首帧前的主题套用：在 <head> 里同步加载，先于样式表生效，
   避免暗色偏好下先闪一帧白底。真正的主题逻辑（跟随系统、原生底色、切换按钮）在 theme.js。

   独立成文件而不是内联：内联脚本一旦遇到严格 CSP（script-src 不含 'unsafe-inline'）会被拦掉，
   而同源外部脚本在 script-src 'self' 下照常执行。文件很小，又是本地资源服务器，不额外拖慢首帧。 */
(function () {
    "use strict";

    try {
        var pref = localStorage.getItem("jabgrab-theme");
        if (pref !== "light" && pref !== "dark") pref = "system";

        var dark = pref === "dark" ||
            (pref === "system" && window.matchMedia &&
                window.matchMedia("(prefers-color-scheme: dark)").matches);

        document.documentElement.classList.toggle("dark", dark);
        document.documentElement.style.colorScheme = dark ? "dark" : "light";
    } catch (e) {
        /* 隐私模式下读不到 localStorage：按亮色照常渲染，theme.js 会再纠正 */
    }
})();
