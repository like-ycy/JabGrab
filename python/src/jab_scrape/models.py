"""数据模型：CLI 输出的 JSON 结构。"""

from dataclasses import asdict, dataclass, field
from typing import Any


@dataclass
class ScrapeResult:
    """单次爬取结果，序列化后即 CLI 的 stdout 输出。"""

    ok: bool
    m3u8_url: str | None = None
    video_name: str | None = None
    headers: dict[str, str] = field(default_factory=dict)
    # 演员名（可能多个 / 可能为空），Go 侧据此决定保存到哪个子目录
    actors: list[str] = field(default_factory=list)
    error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)
