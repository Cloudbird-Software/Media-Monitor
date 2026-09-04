# Media-Monitor 原子能力测试报告 · 第一期（合成站可用性 + 异常流量特征分析）

> 测试对象：`D:/Projects/temp2/oracle/mediamonitor/Media-Monitor`（Go 重写版，只读未改一行业务代码）
> 测试环境：Windows / Git Bash / go1.26.7；合成站 `oracle/replay/synth_api.py`（8661 dy / 8662 xhs / 8663 ks）
> 日期：2026-09-02。**全程零真站访问**：全部流量 127.0.0.1（见 §7 自验）。
> 台架：`oracle/mediamonitor/testlab/`（build_adapt_synth.py / proxy_rec.py / fake_signer.py / run_p0.sh / run_failclosed.sh / run_p1.sh / analyze_traffic.py，captures/*.jsonl 为全部原始取证）。

---

## 1. 构建与适配记录

**构建**：`go build` 一次通过（stdlib-only，无第三方依赖）。产物：
- `mediamonitor/bin/mediactl.exe`（CLI 主入口）
- `mediamonitor/bin/mediad.exe`（守护进程 REST，user-posts 原子经它暴露）
- `mediamonitor/bin/signsvc.exe`（签名服务，stub provider）

**合成契约适配**（`testlab/build_adapt_synth.py` 生成 `testlab/adapt_synth/`，`MEDIAMON_ADAPT_DIR` 指向它）：
- 26 份契约全量复制；三站 `base_url` → 本地记录代理 `127.0.0.1:8771/8772/8773`（proxy_rec.py 前置转发 synth_api 866x，完整落盘每个请求：时序/头序/参数/响应码）。
- 路由对齐 synth_api 实有端点（7 处改写）：xhs-search→POST v2/search/notes；xhs-comments→v2/comment/page；xhs-replies→v2/comment/sub/page；ks-search→POST /rest/v/search/feed（synth 的 /graphql 无 visionSearchPhoto）；ks-comments→POST /rest/v/photo/comment/list（binding→rootCommentsV2、pcursorV2 游标）；douyin-user→user/profile/other；douyin-im-unread→搜索端点（探测廉价契约替身，synth 无 imapi）。
- **签名/cookie 声明原样保留**（dy a_bogus 必需、xhs x-s/x-s-common 头签名、ttwid/web_session/did 必需）——合成站对签名宽容，正好测 fail-closed 与伪造签名透传。
- 受控签名源 `fake_signer.py:8790`（signsvc stub 协议兼容，产伪造 a_bogus / x-s / x-s-common，可切 mode=bad）；错误注入 `inject.json`（off/401/403/empty，路径前缀过滤）。

## 2. P0 翻页读链路可用性（三站合成站）

| # | 链路 | 结果 | 数据 | 游标推进 | 备注 |
|---|---|---|---|---|---|
| t01 | dy search l=20 | ✅ | 20 条/1 页 | — | 单页干净通过 |
| t02 | dy search l=60 | ✅ | 60 条/**1 页** | — | **count=60 单页透传**（畸形参数，见 §5-C） |
| t03 | dy search l=0(无上限) | ❌ | 0 条（**~1980 条被丢弃**） | offset 0→20→…→1980 共 100 页 | **maxPages=100 触顶报错且返回 nil**，55.6s 打满 100 页 |
| t04 | dy comments l=60 | ✅ | 60 条/2 页 | cursor 0→50（count=60 被 synth 钳到 50） | 数值游标链正确 |
| t05 | dy replies l=40 | ✅ | 7 条（该评论共 7 楼） | — | 楼中楼链路通 |
| t06 | dy user-posts l=60（REST） | ✅ | 60 条/2 页 | max_cursor=1788225829999（epoch-ms 链） | 回溯原子游标正确 |
| t07 | xhs search l=20 | ✅ | 20 条/1 页 | — | synth 响应无下一页游标（page 语义），单页为契约天花板 |
| t08 | xhs comments l=30 | ✅ | 21 条（全部）/3 页 | cursor=上页末条评论 id（10/10/1） | 不透明 id 游标链正确 |
| t09R | xhs user-posts l=60（REST） | ✅ | 60 条/2 页 | cursor=末条 note_id（num 钳 50） | 游标续采正确 |
| t10 | xhs replies | ❌ | 0 | — | **适配断点**：引擎发 `comment_id`，synth 要 `root_comment_id`（引擎 API 参数名固定，契约数据层改不了名） |
| t11 | ks search l=60 | ✅ | 60 条/2 页 | pcursor ""→"1"，末页 "no_more" 止链 | pcursorV2/pcursor 数值串 asBool=真、"no_more"=假的链路成立 |
| t12 | ks comments l=60 | ✅ | 13 条（全部）/1 页 | — | 该作品仅 13 评论 |
| t13/t14 | dy/ks user enrich | ❌ | 0 | — | 合成站缺口：dy profile 返回 user 对象（契约要 user_list 批量信封）+参数名 sec_uid≠sec_user_id；ks 无 /api/user/info 端点 |

**判定：翻页读链路 9/9 通**（search 3/3、comments 3/3、user-posts 2/2 可用面、replies dy 通）；3 处断点全部是合成站端点/参数形态缺口而非引擎缺陷（xhs replies 参数名、dy/ks user enrich）。

**签名链路 fail-closed（8 例，Δreq 为代理取证）**：

| 用例 | 结果 | Δreq | 证据 |
|---|---|---|---|
| FC1 dy 无 signer | 拒发 | 0 | `signature required param "a_bogus" missing/empty in final URL` |
| FC2 dy 无 cookie | 拒发 | 0 | 同上门先于 cookie 门触发（签名校验在前） |
| FC3 signer 不可达 | 拒发 | 0 | `dial tcp …connectex` 后不发裸请求（ReturnUnsigned=false 实证） |
| FC4 signer 返回 500 | 拒发 | 0 | `signclient: status 500` |
| FC5 xhs-video + 伪造签名 | **发出** | 1 | **x-s/fake-xs-*、x-s-common 走请求头到达合成站**（非 query），origin/referer 契约头同至；synth 无 feed 端点回 404（预期） |
| FC6 xhs-video 无 signer | 拒发 | 0 | `signature required header "x-s" missing/empty (signer output)` |
| FC7 xhs-video + 真 signsvc stub | 拒发 | 0 | **stub 只产 a_bogus，喂不饱头签名契约**——stub 签名服务与 xhs 类契约不兼容（发现） |
| FC8 dy search + signsvc stub | 通过 | 1 | 20 条；query 参数签名契约与 stub 兼容 |

## 3. P1 账号轮换 / 健康探测（错误注入）

| 用例 | 结果 |
|---|---|
| 401 注入 + auto 轮换 | 3 请求（3 账号各 1 次）→ `auto rotation exhausted after 2 switches`；maxRotations=2 实证 |
| 403 注入 + auto 轮换 | 同上（401/403 同归 ErrAuthWall） |
| 空页注入(200+{}) + auto | ErrEmptyPage 同样触发换号重试（半死 cookie 语义成立） |
| 连续失败封禁 | 3 个账号三轮后全部 `status=banned`（连续 3 败阈值实证） |
| xhs probe 401 | `expired: auth wall: status 401`（真实 HTTP 路径） |
| xhs probe 空页 | `expired: page 1 empty (200 + empty body)`（200+空页按设计归 expired，degraded 留给传输层/5xx） |
| dy probe（任意注入） | **Δreq=0，恒 expired**：`probeEngine` 未注入 Signers（accounts.go:273-289），签名必需契约在 buildURL 就 fail-closed → 探测永远不出网、好号被误判 expired（**缺陷**） |
| xhs probe 正常态 | `expired: page 2 empty at depth`——**探测深度检查硬编码 `probeDepthCursor="20"`**（probe.go:147），对不透明 id 游标（xhs note-id 游标）伪造出非法 cursor → 第二页必空 → **健康账号被误判 expired**（**缺陷**） |

另：CLI `--account auto` 被 `accountPoolFor` 拦截（"not found"），auto 模式只有 mediad REST `account_id:"auto"` 可用——入口不一致（小缺陷）。

## 4. 异常流量特征差距矩阵（平台视角可检测性评级）

基线：`oracle/recording/corpus`（真人三站录制，取每场景前 2 sample：dy 349 / xhs 364 / ks 63 条 API 请求，含完整头集与时序）。实测：captures/*.jsonl（dy 122 / xhs 12 / ks 4 条 P0+P1 全量请求）。

| 维度 | Media-Monitor 实测 | 真人基线 | 平台可检测性 | 证据 |
|---|---|---|---|---|
| **A1 页间间隔** | dy 99 页链 p50=547ms、**σ=36ms（CV 6.6%）**、min469/max657——间隔=纯服务端时延回声，零思考时间零抖动 | 同端点翻页 dy p50 14ms/mean 207ms/**max 2.2s/σ 544ms**；xhs max 6.1s；ks mean 1856ms/**max 28s/σ 5.9s**（重尾分布：簇发+秒级真翻页） | **高**（速率+规律性双特征：匀速巡航曲线本身就是指纹） | captures/douyin.jsonl t03 链 |
| **A2 错误后节奏** | 401/403/空页注入下 9 连发，间隔 **0/0/0/16/16/281/422ms**——换号即重发，无退避 | 真人遇错会停顿/换页面 | **高** | E_retry burst_sizes=[9] |
| **B1 头集** | 均值 **4.0-5.5 个头**；Referer 0%（dy/ks）、Accept-Language 0%、sec-ch-ua 0%（仅 xhs-video 契约带 origin/referer，8.3%） | 均值 **19-28 个头**；Referer≈100%、Accept-Language≈100%、sec-ch-ua 全带 | **高** | §B 头集对照 |
| **B2 UA 池** | 默认 44 条池轮换（accounts ua.js 内嵌）：**全是 Android UA、仅 SM-G981B/SM-G955U 两种机型、Chrome 版本号为伪造乱码**（Chrome/138.2.8.5~875.5.4.8，2026 年真 Chrome≈152）——对着 PC web 端点发移动 UA | 每会话 **1 个**真实桌面 Chrome UA（版本合法） | **高**（版本号正则一层即中） | UA 分布 44 条样本 |
| **B3 cookie-UA 绑定** | 同一 ttwid 在一条翻页链上配过 **46 个不同 UA**（逐请求轮换）；反向：同一 UA 配多 cookie | 会话内 UA/cookie 严格绑定 | **高** | mm_cookie_ua_drift |
| **B4 头序/协议** | `host > user-agent > cookie > accept-encoding`（Go 排序写出）；Accept-Encoding 仅 `gzip` | Chrome h2 序 `:authority/:method/:path/:scheme` + accept/accept-language 等 ~20 头；`gzip, deflate, br, zstd` | 中（需 h2/头序层观测） | header_order 取证 |
| **C1 count 透传** | `query[count]=limit` 直传：实测 **count=60、count=100 单页请求**；`--limit 0` 时不设 count → 默认 20 连打 100 页 | dy 恒 count=20（偶发 50）、xhs num=10/30——从不出现 60/100 | **高**（异常大 count 是强指纹） | t02/t03/并发组 |
| **C2 步进规律** | offset 步进集合恒 {20}；cursor 形态 numeric/opaque 跟随契约（正确） | 真人翻页受 UI 触发，页大小固定但节奏随机 | 中（与 A1 叠加成强信号） | offset_step=[20] |
| **D 并发/突发** | 4 个并行 collect：请求落点 0/7203/7813/7813ms（首请求 7.2s 渲染串行化了后三个；服务端一快即真并发）；引擎并发安全但**无全局 QPS/节流器** | 真人单会话串行+簇发 XHR | 中 | 并发组 Δreq=4 |
| **E 重试/退避** | **MaxRetries=0 实证**：装配层不设退避，429/5xx 单次尝试直接失败或换号；注入错误后 0-16ms 内连发（见 A2）；无 Retry-After 处理 | — | **高**（错误后立即风暴是典型机器人形状） | FC/P1 组 |
| **F TLS/HTTP2 指纹** | Go stdlib `net/http`：无 utls/Chrome ClientHello 模拟（JA3=Go 默认）；h2 帧序/SETTINGS 为 Go 默认；HTTP/1.1 时头序为 Go 排序 | 真人 Chrome TLS+h2 指纹 | **高**（一层识别非浏览器客户端；静态分析结论，本环 http 未触碰 TLS 面） | client.go:76 静态 |

## 5. 改进待办（按 可检测性 × 修复成本 排序，供静默化二期）

| # | 待办 | 可检测性 | 成本 | 位置/做法 |
|---|---|---|---|---|
| 1 | **页间节流+随机抖动**：fetchPagesWith 循环内注入对数正态思考时间（建议 p50≈1.5s，长尾到 8s+），页大小越小越像人 | 高 | 低 | predicate.go:203-233（循环体现无等待） |
| 2 | **请求头集补齐**：契约 transport.headers 增加 referer/accept/accept-language/sec-ch-ua/优先级（三站真值在语料 req_*.json 里可直接抄）；补 cookie jar 让 set-cookie（msToken 轮转）回写 | 高 | 低 | 各契约 JSON + engine.go:275-301 |
| 3 | **count 钳制**：query[count_param] 钳到契约 count_default（20），limit 只用于停页，绝不透传；`--limit 0` 拒绝或强制上限 | 高 | 低 | predicate.go:216-218 |
| 4 | **UA 池重建 + 会话绑定**：换掉 44 条伪造版本号 Android UA（用当前真实 Chrome/Edge 桌面 UA），UA 一旦选定绑定到会话/账号（与 cookie 一对一），禁止逐请求轮换 | 高 | 中 | internal/accounts/uapool.go、httpclient client.go:236 |
| 5 | **退避启用**：CLI/mediad/MCP 装配 MaxRetries≥2+抖动、尊重 Retry-After；auto 换号前先退避（当前 0-16ms 连发） | 高 | 低 | wiring.go:64-66、mediad-mcp main.go:185、rotate.go |
| 6 | **TLS/HTTP2 拟态**：utls Chrome ClientHello + h2 帧序模拟，或经浏览器农场代理出流量 | 高 | 高 | httpclient（新增依赖，违 stdlib-only 约束，需 ADR） |
| 7 | **全局 QPS 治理**：按平台主机域令牌桶，多任务叠加限速 | 中 | 中 | engine/runner 层新增节流器 |
| 8 | **maxPages 行为修正**：触顶时返回已采数据+游标而非 nil；页数上限可配置 | 中 | 低 | predicate.go:246-249（t03 丢弃 ~1980 条） |
| 9 | **probe 修复两处**：probeEngine 注入 Signers（否则 dy 探测永不出网误判 expired）；深度探测 cursor 改用响应 next_cursor 而非硬编码 "20"（不透明游标平台必误判） | 中 | 低 | accounts.go:273、probe.go:147 |
| 10 | **stub signer 补头签名**：signsvc stub 对声明 signature.headers 的契约也产占位头（否则开发环境喂不饱 xhs 类契约） | 低 | 低 | cmd/signsvc stubProvider |
| 11 | 附带：游标哨兵显式化（"no_more" 目前靠 asBool 数值巧合止链，建议引擎显式识别哨兵值）；CLI `--account auto` 放行 | 低 | 低 | predicate.go / wiring.go |

## 6. 复现命令

```bash
cd /d/Projects/temp2/oracle/mediamonitor/testlab
# 服务面（另终端）：synth_api 866x + proxy 877x + fake_signer 8790
D:/Projects/temp2/oracle/env/Scripts/python.exe build_adapt_synth.py
bash run_p0.sh            # P0 三站翻页链路
bash run_failclosed.sh    # 签名 fail-closed 8 例
bash run_p1.sh            # 账号轮换/健康探测（注入 401/403/空页）
D:/Projects/temp2/oracle/env/Scripts/python.exe analyze_traffic.py   # 差距矩阵
```

## 7. 自验与零出网证明

- **复现**：复跑 t04（dy comments l=60）→ exit=0、60 条、2 请求、游标推进 0→50 与首轮一致；逐条 cid 不同是 synthgen 评论 id 为雪花结构内嵌时间分量（设计如此），链路形状完全可复现。
- **零出网**：① 适配目录 26 份契约 base_url 全部= `http://127.0.0.1:877x`（grep 证明）；② captures 三个站点全部请求 client 字段= 127.0.0.1（无一例外）；③ synth_api/proxy/signer/mediad 全部只绑 127.0.0.1（netstat 证明）；④ Media-Monitor 引擎不会自行构造契约外主机（URL=base_url+path 数据拼接）。
