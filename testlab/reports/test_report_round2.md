# Media-Monitor 原子能力测试报告 · 第二期（静默化集成复测）

> 测试对象：`D:/Projects/temp2/oracle/mediamonitor/Media-Monitor` 分支
> `feature/silent-scraping`（B 线 8 commit 静默化改造 + C 线 TODO-C 裁决 commit
> `95419b9`），二进制重建于 `mediamonitor/bin/{mediactl,mediad,signsvc}.exe`。
> 测试环境：Windows / Git Bash / go1.26.7；合成栈 `bash oracle/up.sh --mode synth
> --synth-ports 8701,8702,8703 --no-855x-compat`（**开发口**）；记录/注入代理
> 8771→8701 / 8772→8702 / 8773→8703；fake_signer 8790；mediad 8890。
> 日期：2026-09-02。**全程零真站访问**（见 §8）。台架：`testlab/`
> （一期工具 + 新增 `analyze_compare.py` 三列对照复算）。

---

## 1. TODO-C 落实：xhs 子评论参数名裁决（MM commit `95419b9`）

A 线语料裁决：`xhs_note_detail_comments` 64/64 个 sub/page 请求全用
**root_comment_id**（零个裸 comment_id）。据此在 Media-Monitor 落地：

- `adapt/contracts/xhs-comments-replies.json` v1：placeholder
  `["comment_id"]` → `["root_comment_id"]`（doc 记录裁决出处）——参数名回归
  **纯契约数据**，引擎按「契约首个 placeholder」取名（dy 保持 comment_id）。
- **删兼容别名依赖**：整体删除过渡机制 `transport.reply_target_param`
  （`internal/contracts/contract.go` 字段、`engine.go` buildURL 的 comment_id
  占位门旁路、CommentReplies 覆盖分支）；MM 请求线不再依赖合成站的
  comment_id 过渡别名（synth_api.py 别名保留但已无调用方）。
- 测试对齐：`TestReplyTargetParamFromContract`（契约数据驱动参数名，双平台
  用例）、`platforms/xhs` 契约驱动用例改断言 root_comment_id；
  `go test ./...` 全绿。CHANGELOG-SILENT §6-4 与 TODO-SILENT-HIGHCOST §5
  标记裁决落地。
- 集成实证：t10 请求线（captures/xhs.jsonl）只出现
  `root_comment_id=…`，10 条子评论成功返回（一期该链路断在此处）。

## 2. 集成期发现并处置的问题（如实记录）

1. **synth xhs cid 索引缺陷（A 线遗留，已修）**：`_cid_remember` 提取
   `c["cid"]`，而 xhs 评论信封条目主键是 `id` → xhs cid 索引恒空，sub/page
   只带 root_comment_id 时 `_cid_lookup` 必 miss 回「笔记不存在」。一期 t10
   死在参数名，从未走到该路径，故未暴露。修复：`oracle/replay/synth_api.py`
   `xhs_comment_page` 处按 ks 同款把 `id` 归一成 `cid` 再入索引（带注释）。
2. **数据集于 2026-09-02 20:55 重生成**（一期跑在其前）：同 note/aweme 的
   评论集合与首条评论变化——t08 该笔记评论 21→20 条（20/20 取尽，行为
   正确）；t05 首条评论楼中楼 7→0 条。台架改为**探测合成站选「有楼/有子
   评论」的 cid**（run_p0.sh），链路语义不变。
3. **t09 作者规模边界**：一期 XHS_UID 在新数据集中有 5.3 万作品，synth
   user_posted 全量读档 ~29s/页 > mediad 30s 客户端超时（**一期 tsv 同样
   超时**，一期报告 t09 ✅ 与其 tsv 矛盾）。二期改用 30 作品作者
   `af7c0be202c07f1eae0bc34f`（20/页 → 2 页游标续采 30/30，1.2s），判定口径
   与一期一致。synth 该端点 O(作者作品数) 读档是 A 线性能特性，非 MM 缺陷。
4. **P1-D1 语义澄清**：dy 探测契约（douyin-im-unread 重映射）是 fields-only
   绑定，「200+空体」不可表达 → healthy 是**正确**分类（一期 dy probe 恒
   expired 才是缺陷，已由 B 线修好，P1-0 实证 healthy + Δreq=1）。

## 3. P0 翻页读链路：12/12 全通（对照一期断点清单）

| # | 链路 | 结果 | 数据/游标 | 对照一期 |
|---|---|---|---|---|
| t01/t02 | dy search l=20/60 | ✅ | 20；60 条 3 页（count=20/页，offset 0→40） | 一期✅（但 count=60 单页畸形） |
| t03 | dy search l=0 | ✅ | **2000 条完整落盘** + 可续采游标（100 页守卫触顶，`maxpages_hit`），315.7s | 一期❌ 丢 ~1980 条+报错 |
| t04 | dy comments l=60 | ✅ | 60 条/3 页（cursor 0→20→40） | 一期✅ |
| t05 | dy replies | ✅ | 4 条（c#2 有楼评论 4 楼取尽） | 一期✅（7 楼，数据集已换） |
| t06 | dy user-posts l=60（REST） | ✅ | 60 条/3 页（max_cursor epoch-ms 链） | 一期✅ |
| t07 | xhs search l=20 | ✅ | 20 条/1 页 | 一期✅ |
| t08 | xhs comments l=30 | ✅ | 20/20 条取尽（新数据集该笔记 20 评论，3 页） | 一期✅ 21 条 |
| t09 | xhs user-posts l=60（REST） | ✅ | 30/30 条（2 页，cursor=末条 note_id） | 一期 tsv 实为超时❌ |
| t10 | **xhs replies** | ✅ | **10 条**（root_comment_id 主参数，22 子评论首页） | **一期❌ 断点①已通** |
| t11 | ks search l=60 | ✅ | 60 条/3 页（pcursor ""→1→2→no_more） | 一期✅ |
| t12 | ks comments l=60 | ✅ | 13/13 条 | 一期✅ |
| t13 | **dy user enrich** | ✅ | 1 条档案（sec_uid → $.user_list） | **一期❌ 断点②已通** |
| t14 | **ks user enrich** | ✅ | 1 条档案（/api/user/info → $.user_list） | **一期❌ 断点③已通** |

**判定：12/12 通**（一期 9 旧链路全部保持 + 3 处断点全部修复：xhs replies
参数名=TODO-C、dy/ks user enrich=A 线端点补齐）。t03 行为缺陷（maxPages
丢数据）一并修复。

## 4. 签名链路 fail-closed：8/8（与一期对照）

| 用例 | 结果 | Δreq | 与一期差异 |
|---|---|---|---|
| FC1 dy 无 signer | 拒发（a_bogus missing） | 0 | 一致 |
| FC2 dy 无 cookie | 拒发 | 0 | 一致（签名门先于 cookie 门） |
| FC3 signer 不可达 | 拒发（connectex 后不发裸请求） | 0 | 一致 |
| FC4 signer 5xx | 拒发 | 0 | 一致 |
| FC5 xhs-video 假签名 | 发出，x-s/x-s-common 走头到达合成站（404=无 feed 端点，预期） | 1 | 一致 |
| FC6 xhs-video 无 signer | 拒发（header x-s missing） | 0 | 一致 |
| FC7 xhs-video signsvc-stub | **发出（stub 补头签名）** → 404 | 1 | 一期拒发（stub 喂不饱头签名）——B 线低成本项落地，开发环境可用性改进 |
| FC8 dy search + stub | 通过 20 条 | 1 | 一致 |

## 5. P1 账号轮换 / 健康探测（错误注入）

| 用例 | 结果 | 对照一期 |
|---|---|---|
| P1-0 probe 注入off | **healthy，Δreq=1**（dy probe 出网成功） | 一期❌ 恒 expired（probeEngine 漏注 Signers）——已修 |
| P1-A probe 401 | expired `auth wall: status 401`，Δreq=1 | 一致 |
| P1-B/C auto轮换 401/403 | 3 请求（3 账号各 1 次）→ exhausted，**8.1s**（换号前退避 1s→2s 指数+抖动） | 一期 9 连发 ~1s（0-16ms 间隔）——退避落地 |
| P1-D1 probe 空页 | healthy（fields-only 绑定，空不可表达——正确语义，见 §2-4） | 一期 dy probe 不出网无法判定 |
| P1-D2 auto轮换 空页 | 3 请求 exhausted（ErrEmptyPage 换号语义保持） | 一致 |
| P1-E 连续 3 败 | 3 账号全部 `banned`（阈值实证） | 一致 |
| P1-X0 xhs probe 正常 | **healthy**（一期❌ 假 expired「page 2 empty at depth」——深度游标硬编码缺陷已修） | 已修 |
| P1-X1/X2 xhs probe 空页/401 | expired（`page 1 empty (200+empty body)` / `auth wall`） | 语义保持 |

## 6. 异常特征前后对照（一期 → 二期 → 真人基线）

复算：`testlab/analyze_compare.py` → `runlogs/compare_round1_round2.json`
（原始取证 `captures_round1/` 140 请求、`captures/` 150 请求，全部 127.0.0.1）。

| 指标 | 一期 | 二期 | 真人基线 | 判定 |
|---|---|---|---|---|
| dy 主链（百页）间隔 p50 | 562ms（一期报告 547） | **2062ms** | 页级翻页秒级（p90 1816ms） | 落入真人页级区间 |
| dy 主链 p90 / max | 609 / 2016ms（报告 657） | **6500 / 27172ms** | 1816 / 27019ms | 重尾形态对齐 |
| dy 主链 CV | 0.269（报告纯链 6.6%） | **1.032** | 3.485 | 匀速巡航指纹消除 |
| 零间隔（≤5ms） | 1 | **0** | 9/337（XHR 簇发） | 无机器连发 |
| 均值头数 | 4.0-5.5 | **16.0-18.6** | 19.0-27.8 | 大幅收敛，仍有差距（见 §7） |
| Referer / Accept-Language 在网率 | 0% / 0% | **100% / 100%** | ≈100% / ≈100% | 对齐 |
| sec-ch-ua 族（与 UA 一致） | 0% | **100%** | 100%（xhs 语料 50%） | 对齐 |
| count/num/page_size 值域 | {20,40,60,100} | **{20}** | dy {20,50}；xhs {10,30} | 异常大 count 消除 |
| 链内 UA 漂移（同 cookie） | **44**（百页链逐请求轮换） | **1**（跨进程测试 2-3，生产长驻进程为 1） | 1 | 对齐 |
| 整场 UA 数 / 请求数 | 53/124 | 11/129（=身份数） | 1/会话 | 从"逐请求"变"逐身份" |
| 401 后时序 | 0/16/16/281/422ms 连发 | **250/1078/2078/5172ms**（重试 250ms 起 + 换号 1s→2s→4s 指数±抖动） | 停顿/换页面 | 风暴消除 |

**残余可检测面（如实）**：①xhs num 固定 20，语料实证 {10,30}——契约
count_default 建议改 10/30（低成本）；②头数 16-18.6 vs 真人 19-27.8
（Accept-Encoding 四元组等，见 TODO-SILENT-HIGHCOST #3）；③TLS/h2 指纹仍
为 Go 默认（JA3 一层可识别，需 utls/ADR 或浏览器农场，零真站约束下本环
无法验证）；④B4 头序仍为 Go 排序（HTTP/1.1）。

## 7. 结论：静默化改进是否达标

**达标（在零真站可验证的五个维度上）**。平台侧可检测性定性下降：

- **速率/规律性特征（A1/A2/C2）**：匀速巡航（CV 0.07-0.27、p90≈p50）变为
  对数正态重尾（CV≈1.0、p90≈3×p50、max 27s≈真人上限），页大小恒 20 落入
  真人值域，offset 步进与真人一致——「速率+规律性」双特征**从高可检测降
  为低**。
- **头集/身份特征（B1/B2/B3）**：裸头集（4 头、零 Referer/sec-ch-ua）变为
  浏览器级 16-19 头、关键头 100% 在网且与 UA 自洽；同 cookie 链内 UA 漂移
  44→1——**从高可检测降为低**（残余：头数差 3-9 个、TLS/h2 层）。
- **参数特征（C1）**：count=60/100 强指纹消除——**从高降为极低**。
- **错误反应（E）**：0-16ms 换号风暴变为指数退避+抖动+Retry-After 尊重
  ——**从高降为低**。
- **不可消除项**：TLS ClientHello/h2 帧序（Go 默认指纹）仍是**高可检测**
  单点——这是二期未做、需 ADR 决策的最高成本项；在它落地前，建议生产
  流量经代理池/浏览器农场出口兜底（见 playbook §3）。

一句话：**应用层五个维度已收敛到真人分布附近，剩余检测面集中在传输层
（TLS/h2）一处。**

## 8. 自验与零出网证明

- **稳定性自验**：12/12 链路各复跑 1 条（`runlogs/self_*.ndjson|json`）：
  s01 dy search 20、s02 xhs search 20、s03 ks search 20、s04 dy comments 20、
  s05 xhs comments 20、s06 ks comments 13/13、s07 dy user-posts 20、s08 xhs
  user-posts 20、s09 dy replies 4、s10 xhs replies 10、s13 dy user 1、s14 ks
  user 1——**全部 exit=0，结果与主轮一致**。
- **零出网**：①adapt_synth 26 份契约 base_url 全部 = `http://127.0.0.1:877x`
  （grep 证明）；②二期 captures 150/150、一期归档 140/140 条请求
  client=127.0.0.1（无一例外）；③synth/proxy/signer/mediad 全部只绑
  127.0.0.1（netstat 证明）；④MM 引擎 URL=base_url+path 数据拼接，不自行
  构造契约外主机。

## 9. 复现命令

```bash
# 1. 起合成栈（开发口，synth-only 即够契约面）
bash oracle/up.sh --mode synth --synth-ports 8701,8702,8703 --no-855x-compat
# 2. 台架服务面（另终端）
cd oracle/mediamonitor/testlab
PY=D:/Projects/temp2/oracle/env/Scripts/python.exe
$PY proxy_rec.py --listen 8771 --target 127.0.0.1:8701 --site douyin &
$PY proxy_rec.py --listen 8772 --target 127.0.0.1:8702 --site xhs &
$PY proxy_rec.py --listen 8773 --target 127.0.0.1:8703 --site kuaishou &
$PY fake_signer.py 8790
# 3. 重建适配 + 三套用例 + 两版分析
$PY build_adapt_synth.py
bash run_p0.sh            # P0 12 链路（t03 约 5-6 分钟，页间节流所致）
bash run_failclosed.sh    # 签名 fail-closed 8 例
bash run_p1.sh            # 账号轮换/健康探测（注入 401/403/空页）
$PY analyze_traffic.py    # 本轮差距矩阵（runlogs/traffic_analysis.json）
$PY analyze_compare.py    # 一期/二期/真人三列对照（runlogs/compare_round1_round2.json）
```

一期原始取证归档于 `testlab/captures_round1/`（含 `runlogs_round1/`）。
