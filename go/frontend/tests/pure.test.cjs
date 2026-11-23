/* 前端纯函数测试：node --test frontend/tests/
   与浏览器共用 dist/pure.js 同一份实现，不启动 DOM。 */

const test = require("node:test");
const assert = require("node:assert/strict");

const { normalize, humanSize, baseName, dirName, escapeHtml, summarize, clone } =
    require("../dist/pure.js");

test("normalize 补齐缺省字段", () => {
    const c = normalize({});
    assert.equal(c.saveDir, "");
    assert.equal(c.txtPath, "");
    assert.deepEqual(c.recentTxt, []);
    assert.equal(c.componentsOnDemand, true);
    assert.deepEqual(c.sites, {});
    assert.deepEqual(c.download, {});
    assert.equal(c.scrapeTimeoutSec, 180);
});

test("normalize 保留已有值，false 不被当成缺省", () => {
    const c = normalize({
        saveDir: "/data",
        componentsOnDemand: false,
        scrapeTimeoutSec: 60,
        download: { threads: 8 },
        sites: { jable: { enabled: true, keyword: "jable" } },
    });
    assert.equal(c.saveDir, "/data");
    assert.equal(c.componentsOnDemand, false);
    assert.equal(c.scrapeTimeoutSec, 60);
    assert.equal(c.download.threads, 8);
    assert.equal(c.sites.jable.keyword, "jable");
});

test("normalize 是深拷贝：改结果不影响入参", () => {
    const src = { sites: { jable: { enabled: true } }, recentTxt: ["a"] };
    const c = normalize(src);
    c.sites.jable.enabled = false;
    c.recentTxt.push("b");
    assert.equal(src.sites.jable.enabled, true);
    assert.deepEqual(src.recentTxt, ["a"]);
});

test("normalize 容忍 null/undefined", () => {
    assert.equal(normalize(null).saveDir, "");
    assert.equal(normalize(undefined).txtPath, "");
});

test("humanSize 边界与单位", () => {
    assert.equal(humanSize(0), "");
    assert.equal(humanSize(-5), "");
    assert.equal(humanSize(null), "");
    assert.equal(humanSize(1024 * 1024), "1.0 MB");
    assert.equal(humanSize(1.5 * 1024 * 1024), "1.5 MB");
    assert.equal(humanSize(1024 * 1024 * 1024), "1.0 GB");
    assert.equal(humanSize(2.09 * 1024 * 1024 * 1024), "2.1 GB");
});

test("baseName / dirName 兼容两种分隔符", () => {
    assert.equal(baseName("/a/b/c.mp4"), "c.mp4");
    assert.equal(baseName("C:\\Videos\\c.mp4"), "c.mp4");
    assert.equal(baseName("c.mp4"), "c.mp4");
    assert.equal(dirName("/a/b/c.mp4"), "/a/b");
    assert.equal(dirName("C:\\Videos\\c.mp4"), "C:\\Videos");
    assert.equal(dirName("c.mp4"), "c.mp4");
});

test("escapeHtml 覆盖全部敏感字符与空值", () => {
    assert.equal(escapeHtml(`<a href="x">&'</a>`), "&lt;a href=&quot;x&quot;&gt;&amp;&#39;&lt;/a&gt;");
    assert.equal(escapeHtml(null), "");
    assert.equal(escapeHtml(undefined), "");
    assert.equal(escapeHtml(123), "123");
});

test("summarize 三段汇总", () => {
    const items = [
        { status: "done" },
        { status: "skipped" },
        { status: "failed" },
        { status: "cancelled" },
        { status: "downloading", percent: 40 },
        { status: "pending" },
        { status: "scraping" },
    ];
    const s = summarize(items);
    assert.equal(s.ok, 2);
    assert.equal(s.bad, 2);
    assert.equal(s.cur, 0.4);
    assert.equal(s.current, items[4]);
});

test("summarize 百分比越界被夹到 1", () => {
    const s = summarize([{ status: "downloading", percent: 150 }]);
    assert.equal(s.cur, 1);
});

test("summarize 空列表与缺省状态", () => {
    assert.deepEqual(summarize([]), { ok: 0, bad: 0, cur: 0, current: null });
    assert.deepEqual(summarize(null), { ok: 0, bad: 0, cur: 0, current: null });
    assert.equal(summarize([{}]).ok, 0);
});

test("clone 与 structuredClone 行为一致", () => {
    const src = { a: [1, { b: 2 }] };
    const c = clone(src);
    c.a[1].b = 9;
    assert.equal(src.a[1].b, 2);
});