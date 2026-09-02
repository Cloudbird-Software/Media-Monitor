"""synthgen —— 三站合成真站 oracle · 阶段 4 合成数据生成器。

以 Faker(zh_CN) 为基座，按阶段 2 草稿契约生成抖音/小红书/快手核心对象合成数据：
- 分布引擎可配置（distributions.yaml）
- 异常类注入（真实标签仅写入独立 ground_truth.db，绝不进站点数据本体）
- 全链路种子化可复现，支持 --filter 条件过滤子集
"""

__version__ = "1.0.0"

SITE_CODES = {"douyin": 1, "xhs": 2, "kuaishou": 3}
SITES = ("douyin", "xhs", "kuaishou")
