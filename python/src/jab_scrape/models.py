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
    error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)
