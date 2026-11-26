# -*- mode: python ; coding: utf-8 -*-
"""PyInstaller 配置：爬取程序（onedir）。

为什么必须是 onedir，不能用 onefile
----------------------------------
onefile 每次启动都把载荷解压到新的临时目录，seleniumbase 会以为「没有驱动」
而重新下载一次；「单文件 + 拉起浏览器 + 自动联网下载」也是 Windows Defender
的高危特征组合，产物容易被直接删掉。

构建
----
由 tools/build.sh 调用（不要手写 pyinstaller 参数，四仓共用同一套入口）：

    tools/build.sh            # 标准包
    tools/build.sh --full     # 整包

产物：python/build/pyi/dist/jab-scrape/{jab-scrape,_internal/}

体积：两处收紧，都是有实测依据的（详见 docs/release-split-plan.md）
---------------------------------------------------------------
① 剔除 selenium 自带的三份 Selenium Manager（四平台共 16.9MB）。
   **SeleniumBase 不会调用 Selenium Manager**：它始终显式把驱动路径传给
   ChromeService，selenium 那条「没给路径 → 问 Selenium Manager」的分支进不去。
   注意这些文件在两个地方各有一份 —— `a.binaries` 与 `a.datas`，只清一处会省下
   不到三分之一，很容易误判成「已经清干净了」。

② 清空 seleniumbase/drivers/：SeleniumBase 在运行时把驱动下载进
   site-packages/seleniumbase/drivers/，--collect-all 会把该目录原样收进产物。
   于是同一份源码在「跑过爬取的机器」上打出 70MB、在干净的 CI 上打出 35MB，
   版本号相同内容不同，还会把本机驱动版本塞给所有人。驱动必须与本机 Chrome
   匹配，所以发布包不带驱动，首次运行时由 SeleniumBase 自己下。

③ `optimize=2` 去掉 docstring 与 assert。已核实 seleniumbase 4.57 源码里没有
   `assert` 语句，core/ 与 fixtures/ 也不读 `__doc__`。线上若出现异常行为，
   删掉 optimize 就是第一个该回退的开关。
"""

import os
import sys

from PyInstaller.utils.hooks import collect_all

# 控制台编码兜底：Windows runner 默认 cp1252，下面带中文的进度打印会直接抛
# UnicodeEncodeError 把构建打断。
try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    sys.stderr.reconfigure(encoding="utf-8", errors="replace")
except (AttributeError, ValueError):
    pass

# 字节码优化档位。0=默认，1=去 assert，2=再去 docstring。
_OPTIMIZE = 2

# 原 pyinstaller.args 里的 --collect-all：seleniumbase 大量动态导入，
# 静态分析抓不全，必须整包收集数据、二进制与子模块。
_datas = []
_binaries = []
_hiddenimports = []
for _pkg in ("seleniumbase", "mycdp", "pdbp"):
    _d, _b, _h = collect_all(_pkg)
    _datas += _d
    _binaries += _b
    _hiddenimports += _h

a = Analysis(  # noqa: F821
    [os.path.join(SPECPATH, "entry.py")],  # noqa: F821
    pathex=[os.path.join(SPECPATH, "..", "src")],  # noqa: F821
    binaries=_binaries,
    datas=_datas,
    # 站点解析器是惰性导入的（见 src/jab_scrape/__main__.py 的 SOURCE_MODULES）：
    # 为了 --version 在 seleniumbase 装坏时也能给出答案，顶层不静态 import。
    # 代价是静态分析看不到这两个模块，必须显式声明 —— 否则产物里没有它们，
    # 而 --help / --version 一切正常，真正爬取时才 ModuleNotFoundError。
    hiddenimports=_hiddenimports + ["jab_scrape.jable", "jab_scrape.missav"],
    hookspath=[],
    runtime_hooks=[],
    # 只排除确定用不到的 GUI 工具包与测试框架。
    excludes=["tkinter", "_tkinter", "pydoc", "pytest", "_pytest", "behave"],
    noarchive=False,
    optimize=_OPTIMIZE,
)

# ── 体积收紧（理由见文件顶部 ①②）─────────────────────────────────────
_MANAGER_MARKERS = ("selenium/webdriver/common/", "selenium-manager")
_DRIVER_PREFIX = "seleniumbase/drivers/"


def _drop_unwanted(entries):
    kept = []
    for entry in entries:
        dest = entry[0].replace("\\", "/")
        if all(marker in dest for marker in _MANAGER_MARKERS):
            print(f"[pyinstaller.spec] 剔除 {dest}")
            continue
        if dest.startswith(_DRIVER_PREFIX) and not dest.endswith("__init__.py"):
            print(f"[pyinstaller.spec] 剔除运行时驱动 {dest}")
            continue
        kept.append(entry)
    return kept


a.binaries = _drop_unwanted(a.binaries)
a.datas = _drop_unwanted(a.datas)

pyz = PYZ(a.pure)  # noqa: F821

exe = EXE(  # noqa: F821
    pyz,
    a.scripts,
    [],
    exclude_binaries=True,
    name="jab-scrape",
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    # 关闭 UPX：压缩后更容易被杀软误判，对启动速度也没什么帮助。
    upx=False,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
)

coll = COLLECT(  # noqa: F821
    exe,
    a.binaries,
    a.datas,
    strip=False,
    upx=False,
    name="jab-scrape",
)
