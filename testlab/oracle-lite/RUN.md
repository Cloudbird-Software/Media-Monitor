# oracle-lite —— 三站合成真站样本站点（审阅者本地测试台架）

目的：**无需 oracle 全库**（原始台架含 2.7GB 预置数据集与录制语料），在克隆内
三步起一个「抖音/小红书/快手」合成真站样本，并跑一条静默化前后对照验证。
全部监听 127.0.0.1，不访问任何真站。

配套文档：测试证据与二期对照见 `testlab/reports/`（一期实测报告、二期复测
报告、渐进部署 playbook）；改动清单见根目录 `CHANGELOG-SILENT.md`。

## 依赖

- Python 3.11+，`pip install numpy Faker PyYAML`（`synthgen/validate.py` 另需
  `scipy pandas`，仅验收时用）
- Go（仓库本体，仅第三步对照验证用）

## 目录结构

```
testlab/oracle-lite/
├── RUN.md                 本文件
├── synth_api.py           合成站 API 服务（改：默认路径指向包内、端口独立段 876x）
├── synthgen/              合成数据生成器（整包自包含）
│   ├── generator.py       生成 CLI（种子化可复现）
│   ├── validate.py        数据集验收自测（105 项检查）
│   ├── render.py          契约形态响应组装（synth_api 的渲染层）
│   ├── sites/ data/ distributions.yaml ...
│   └── datasets/          预生成迷你数据集（300 条/站，seed=20260902）
├── contracts/             validate 所需 4 个契约文件（字段交叉核对子集）
├── fixtures/              R6 契约差集端点的静态常量（emoji 名录/筛选层等公开目录数据）
└── pages/
    ├── build_pages.py     页面骨架生成（改：脱敏语料缺失时跳过 DOM 对照）
    └── out/               预生成三站页面骨架（home/search/detail/profile）
```

## 三步跑起样本站点

### 第 1 步：生成（或复用）迷你数据集

包内已预生成（300 条/站，固定种子，`validate.py` 验收 101/105，4 只不过项均为
迷你集小样本统计涨落——见下文「验收口径」）；重生成：

```bash
cd testlab/oracle-lite
python synthgen/generator.py --site all --count 300 --seed 20260902 --with-index
python synthgen/validate.py --datasets-dir synthgen/datasets --repro-count 300
```

体积口径：douyin ≈2.6MB / xhs ≈5.0MB / kuaishou ≈1.5MB（含 index.db 与
ground_truth.db）。同种子重生成逐字节一致（repro.same_seed 检查项）。

### 本数据集体现的修复点（红队第 5 ~ 12 轮同步）

本包的 synthgen / synth_api / pages 已同步**红队第 5 ~ 12 轮修复**（R5A~R12C，
2026-09-03 ~ 09-04；R5 前 7 项见下表，R6~R12 代表项随其后），迷你数据集
（300 条/站、seed 20260902）与 validate 105 项检查直接体现以下前后对照：

| 修复项 | 旧实现（同步前） | 本数据集 |
|---|---|---|
| R5A-P1-1 作者池长尾 | 300 条仅 ~35 个不同作者、top1 占 55%（旧 Zipf(1.8) 幂律） | 300 条 260/254/260 个不同作者（dy/xhs/ks）、top1 ≤2.3%；完整台架 10 万条/站为 11711/11788 个、top1 ≤1.02%（组合昵称池 ~3 万、长尾作品数 + 头部份额上限） |
| R5A-P1-2 评论零重复 | 单视频 20 条评论仅 12 种文本、8 组一字不差 | 评论文本模板×槽位组合生成（commentext.py）：抽样的单内容窗口 0 完全重复（xhs 全量嵌入 ~0.3%，仍在语料 98% 唯一率口径内） |
| R5A-P2-2 ks 评论计数 | commentCountV2 全集 {0..21}、62% 零评论（误用 us_c） | us_c 恒 0（语料 100/100）；commentCountV2 独立对数正态，抽样中位 1499（语料中位 1544、区间 [51,28863]） |
| R5A-P2-5 评论者可回查 | 评论用户 → 作者主页 5/5 断链 | 抽样 772/513/800 条评论的评论者 0 断链、昵称跨端点一致 |
| R5B-P1 dy 作者主页 | SSR works 恒空、无作品/喜欢 tab | /user/<sec_uid> SSR works/like_works 非空、「作品/喜欢」tab 可用 |
| R5C-P2-1 错误卫生 | POST body 类型非法 → HTTP 500 + Python traceback | 逐端点容错回 200 业务信封（`page:"abc"` 等 → 缺省值），不泄解释器特征 |
| R5C-P2-2 关键词语义 | 乱码/数字/emoji 词 → 三站空态（与 live 真值相反） | 任意非空关键词按轮转窗口出结果（`qzwkjxvbpq` → 20 卡）；仅空词/越界/count=0 走真站空态族 |

validate 中对应的检查项：`author.longtail` / `author.window_repeat` /
`comment.window_text_unique` / `comment.commenter_resolvable` /
`ks.comment_count_dist` / `ks.us_c_always_zero`。其中 `author.longtail` 的
distinct/top1 阈值按迷你集规模自适应（完整台架 10 万条时与原阈值 ≥5000/≤2%
等价），`comment.window_text_unique` 的单窗口重复上限按语料唯一率取口径
（dy 语料 100% 唯一仍要求 0；xhs/ks 语料本身 98%/94%）。

红队第 6 ~ 12 轮同步的代表修复（synthgen 整包 / synth_api.py / pages /
fixtures 均为本轮同步；完整 24 分册报告在 oracle 侧 `verify/`）：

| 轮 | 修复项 | 旧实现（同步前） | 本包 |
|---|---|---|---|
| R6 | ks GraphQL `visionCommentList` 键集/游标键位（R5A-P2-3） | 缺 `__typename/rootCommentsV2/pcursorV2`，续页游标放 `pcursor` | 信封对齐契约 7 键，`pcursor` 恒 null、续页在 `pcursorV2`（28/28 语料实证） |
| R6 | 契约差集端点补面 12 个（R6A C-2） | dy `suggest_words`/`emoji/list`/`mix`/`series`/`multi`/`profile/self`、xhs `search/recommend`/`filter`/`onebox`/`homefeed/category`/`feed`/`widgets` 全 404 | 形态按契约渲染；静态常量随包（`fixtures/round6_static_payloads.json`，公开 emoji 名录/筛选层） |
| R6 | 空关键词统一空态族（R6C-P3-1）/ backlog 512（R6C-P3-3） | 空词行为三站各异；高并发拒连 | dy `data=[]`、xhs 无 items 键、ks `feeds=[]+no_more`；50 并发 0 拒连 |
| R7 | 关键词→类目映射（R6A-P2-1） | 搜索结果与关键词零耦合（严格命中 ~0） | `search.relevance` 类目耦合 0.60-0.66（≥0.5，语料口径）；ladder 14 词全映射 |
| R7 | dy 搜索上下文隐藏面（R6C-P2-1） | `play_count` 有值（真站搜索上下文恒 0） | `dy.play_count_hidden_true_site` PASS：stream/single/item 61 卡 play_count=0、detail 有值（语料 849/849） |
| R7 | 评论延迟长尾/文本风格带（R6A-P2-2、R7A-P3-2/3） | 评论时间即时爆发（p50 0.03 天）、语气词率 0.40-0.59 | `comment.delay_longtail` p50 5.9-6.5 天（语料 8.1）、语气词/网络用语/@提及进 ±30% 带（完整台架口径） |
| R8/R9 | **ks 评论 id / 子评论 id 全局唯一**（R8A-P2-1、R9A-P2-1） | ks 评论 id 照片局部重启，跨照片碰撞 | id/游标掺 photo_id 种子、子评论掺 root 序号——跨照片/跨 root 唯一（rt8fix 39/39、rt9fix 34/34 同源代码） |
| R9 | 子-根评论时间因果（R9A-P2-2） | 子评论时间可早于根评论（51% 违例） | 子时间 = 根时间 + 正偏移（语料公理 0/168 违例，0 违例） |
| R8/R10 | JSON 序列化紧凑化（R8C-P3-1）+ dy/ks 转义表（R10A-P3-1） | 体带空格分隔；dy raw `&` 外露、ks emoji 原生 | 全出口 `separators=(",",":")`；dy `&<>`/U+2028 转义为 `\uXXXX`、ks REST emoji 转代理对 |
| R10 | 媒体 URL 按次签发盐（R10A-P3-4）+ GET/HEAD 带体排空（R10C-P3-2） | 同实体跨端点媒体 URL 恒同串；带体 GET 打断 keep-alive | 同端点恒同串、跨端点必异；body 排空后连接复用洁净 |
| R11 | **页面路由幻影 404**（R11C-P1-1） | 别名路由真响应后同连接追加第二个 404 站点页（curl 双 URL 第二请求必 404） | `_send` 哨兵防二次发包——`curl 页面 任意URL` 第二响应 200（8/8 别名路由；rt11fix 62/62 同源代码） |
| R12 | dy 可选键族三态携带率（R12A-P3-3） | 实体级可选键族携带率与语料断裂 | `sites/templates/dy_optfam.json`（随包脱敏）按语料众数携带率物化（携带=键在/不携带=键缺/例外 null） |
| R12 | SSR wire 表单 value 恒空（R12A-P3-1）+ 404/根路由落点（R12C-P3-2~5） | 输入框预填关键词；404 落点状态码/路由形态与语料断裂 | 语料六面 value="" 恒空；三站 404 页与根路由对齐语料（含 ks `/404` 200 页） |

### 验收口径（105 项检查）

`validate.py` 自完整台架同步后为 **105 项**检查；迷你集（300 条/站）实跑
**101/105**，规模自适应与不过项说明：

- **阈值按规模自适应**（完整台架 10 万条时与原阈值等价）：`author.longtail`
  （≥min(5000, 80%×n) / top1 ≤2%+2/n）、`search.relevance` 与
  `search.ladder_words_mapped` 的供给阈值（≥24 → 按记录数等比缩放；严格命中
  下带在小供给下退化为 ≥0，类目耦合 ≥0.5 上带照常生效）、`ks.duration_variance`
  （≥min(900, 90%×n)）、`comment.deep_list_unique` 扫描上限（min(2000, n)）。
- **4 只不过项均为 300 条小样本统计涨落**，非缺陷（同一代码在完整台架 10 万条
  105/105 全绿，报告存档于 oracle 侧 `synthgen/datasets/validation_report.json`）：
  dy/xhs `comment.text_style_band`（网络用语 +32%、@提及 +81%——按内容聚类的
  评论文本风格率，迷你集仅 40 内容聚类单元）、xhs
  `dist.like_lognormal_quantiles`（q05 尾分位偏差 6.4% vs 带宽 5%）、xhs
  `time.publish_rhythm`（星期 max 0.220 vs 0.21，n=300 的 ±2σ 涨落）。

### 第 2 步：起合成站（三站一次拉起）

```bash
python synth_api.py --site all
# [synth_api] listening: douyin=127.0.0.1:8761, xhs=127.0.0.1:8762, kuaishou=127.0.0.1:8763
```

端口为 oracle-lite 独立段 **876x**（完整 oracle 实测台架占用 866x/855x，互不
冲突；如需对齐 harness HOST_REWRITE 可 `--also-bind 8551,8552,8553`）。

抽查（健康检查 + 三站契约形态响应 + 页面）：

```bash
curl -s http://127.0.0.1:8761/_synth/health
# dy 关键词搜索（翻页链：cursor 递增 + extra.logid 回传 search_id）
curl -s "http://127.0.0.1:8761/aweme/v1/web/general/search/single/?keyword=%E7%BE%8E%E9%A3%9F&offset=0&count=5&search_id=x"
# xhs 笔记搜索（POST body 12 必填项）
curl -s -X POST http://127.0.0.1:8762/api/sns/web/v2/search/notes -H "Content-Type: application/json" \
  -d '{"keyword":"美食","page":1,"page_size":20,"search_id":"a","note_index":0,"covers_scope":"","prepend":"","note_type":0,"ext_flags":[],"image_formats":["jpg","webp","avif"],"search_firsthit":0}'
# ks 视频搜索（pcursor ""→"1"→"2" 翻页）
curl -s -X POST http://127.0.0.1:8763/rest/v/search/feed -H "Content-Type: application/json" \
  -d '{"pcursor":"","kpn":"PC_WEB","keyword":"美食"}'
# 合成站页面骨架（另有真站 URL 别名：/search/<kw>、/explore/<id>、/search/video?searchKey=）
curl -s http://127.0.0.1:8761/ | head -20
```

### 第 3 步：静默化前后对照验证（一条命令）

```bash
cd <仓库根>
go test ./internal/collect -run TestSilentMockChainReport -count=1
```

该测试对回环 mock 站走 25 页翻页链（节流全开），断言并落盘报告
`quality/silent-mockchain-report.md`，四指标即「二期 vs 一期」对照：

| 指标 | 一期实测（testlab/reports/test_report_round1.md） | 二期断言 |
|---|---|---|
| 页间间隔 | 背靠背 0 间隔（CV≈6.6% 匀速巡航指纹） | p50=320ms（配置 300ms）、p90/p50≈4.07 对数正态重尾、CV=1.11 |
| 请求头数 | 4-5 个 | 最小 15 个（sec-ch-ua 族/referer/cookie 每请求在网） |
| count 参数 | =调用方 limit 透传（limit=500 → count=500 畸形） | 恒 20（limit=500 不再透传） |
| 会话内 UA | 同链 46 个 UA 漂移 | 25 请求 1 个 UA（会话级绑定） |

生产尺度间隔分布（p50=1.5s / p90≈5.4s / p99≈15s）由
`TestLognormalSleepDistribution` 以统计断言覆盖（同包 `go test` 默认全跑）。

## 与完整 oracle 的差异（oracle-lite 变更点）

1. **端口独立段**：默认 8761/8762/8763（原 8661-8663），避免与完整台架冲突。
2. **keyword 轮转窗口自适应**：模数 = min(400, 数据集页数)。完整台架数据集
   ≥8 万条/站，固定 400 页（8000 条）窗口永不出界；迷你集 300 条 → 15 页窗口，
   公式不变仅模数随 MANIFEST count 确定，确定性保持（换关键词结果集仍刷新）。
3. **build_pages 跳过脱敏 DOM 对照**：脱敏录制语料不随包分发（含个人信息，
   不入公开仓）；`--sanitized <dir>` 可恢复完整校验。pages/out 为预生成产物。
4. **契约仅含 4 个文件**（validate 字段交叉核对所需的搜索/评论契约），完整
   契约库在 oracle 侧。
5. **数据集 300 条/站**（完整台架 10 万条/站 ≈2.7GB）；异常类占比、形态不变量
   等验收项在全绿口径下抽查通过。
6. **模板与契约示例值已脱敏清洗**：完整台架的 sites/templates/*.json 提取自
   真站录制（叶子值=真实内容）、contracts 的 `*.example` 为录制响应示例值；
   入包前做了形态保持的确定性替换（昵称→通用池、URL 保留 host 扰动路径、
   id/hex/b64 同形扰动、CJK 自由文本→等长通用文案；种子 20260902）。
   清洗只动值不动结构——渲染层（render.py）对 URL/时间/pii/id 类本就在运行时
   重造，数据集字段经绑定优先透出，故 validate 101/105（4 只统计涨落见上）
   与端点冒烟不受影响。
