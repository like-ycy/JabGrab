"""版本号的唯一来源。

CI 打进发布包之前会覆盖这一行（见 .github/workflows/release.yml 的「注入版本号」）：

    1.2.3 → jab-scrape --version

所以这个文件是**输入**，`jab-scrape --version` 的输出是**结果**。清单里记录的
版本必须取自后者 —— 两者分叉时（漏注入、打包缓存），用户装完会一直停在
「可更新」，而构建一片绿。
"""

__version__ = "0.1.0"
