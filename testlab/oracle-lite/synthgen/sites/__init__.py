"""三站核心对象生成器子包。"""
from . import douyin, kuaishou, xhs

SITES = {"douyin": douyin, "xhs": xhs, "kuaishou": kuaishou}
