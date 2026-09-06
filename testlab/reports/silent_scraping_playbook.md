# 《静默抓取方法学》（Silent Scraping Playbook）

> 依据：`oracle/mediamonitor/test_report_round1.md`（一期差距矩阵）与
> `test_report_round2.md`（二期集成复测 + 一期/二期/真人三列对照）。
> 代码基线：Media-Monitor `feature/silent-scraping`（B 线 8 commit + C 线 TODO-C
> 裁决 commit `95419b9`）。全部机制默认开启，零配置即生效；每个旋钮都有
> 「关到一期行为」的退路（紧急模式/0 值）。
> 适用范围：对平台 Web API 的**读链路**采集（search/comments/replies/user）。

---

## 0. 一句话总纲

**让采集流量在「速率与节奏、请求头集、会话身份、参数形态、错误反应」五个
维度上落入真人观测分布之内，且绝不因为抓取本身制造平台侧的可检测异常。**
静默化不是单一开关，而是七条纪律的叠加；任何一条破了功，其余努力都会被
那一个维度出卖。

---

## 1. 七条原则 → 已实现机制（代码位置 / 配置 / 默认值）

### 原则 1：节奏拟人化（页间思考时间）

**问题**（一期 A1）：引擎页间间隔=纯服务端时延回声，100 页链 p50=547ms、
CV 6.6% 的匀速巡航曲线本身就是机器人指纹。

**机制**：翻页循环内注入对数正态「思考时间」，中位数 1500ms、对数空间
σ=1.0（重尾），钳制 [250ms, 30s]（上限对标语料真人 ks max 28s）。
→ p50=1.5s、p90≈5.4s、p99≈15s。

- 代码：`internal/collect/pacing.go`（`DefaultPacing`）；循环挂点
  `internal/collect/predicate.go`（fetchPagesWith 页间 sleep）。
- 配置：`MEDIAMON_PAGE_SLEEP_MS`（默认 1500，`0`=关闭）；
  `MEDIAMON_PAGE_SLEEP_SIGMA`（默认 1.0）；
  契约级 `paging.page_sleep_ms`（正数=覆盖中位数，`-1`=仅该契约关闭）。
- 紧急模式：`MEDIAMON_EMERGENCY=1` 一键恢复"尽快翻页"（事故/赶时效用，
  用完必须关）。
- 观测：`collect.page_sleep_total_ms` 累计值。

### 原则 2：头集完整性（浏览器级请求头）

**问题**（一期 B1）：只发 4-5 个头，Referer 0%、Accept-Language 0%、
sec-ch-ua 0%；真人 19-28 个头、Referer≈100%。

**机制**：三站平台默认头集（Accept / Accept-Language / Accept-Encoding /
Referer / Origin / Sec-Fetch-* / Priority / Cache-Control / Pragma，形态取自
录制语料的 XHR 请求），引擎按优先级合并：
平台默认 < 签名头（x-s/x-s-common）< 契约 `transport.headers`（逐项可覆盖）
< UA 一致 sec-ch-ua 族 < Cookie/UA。`sec-ch-ua` / `sec-ch-ua-mobile` /
`sec-ch-ua-platform` 由**所发 UA 实时派生**（版本/品牌/平台三者永不打架）。

- 代码：`internal/platforms/{douyin,xhs,kuaishou}/browserhdr.go`、
  合并逻辑 `internal/collect/browserhdr.go`；装配点 mediactl（collect/probe）、
  mediad、mediad-mcp 四处注入 `BrowserHeaders`。
- 实测（二期）：均值头数 16.0-18.6（一期 4.0-5.5；真人 19.0-27.8），
  Referer/Accept-Language 100% 在网。

### 原则 3：会话一致性（cookie jar + UA 绑定）

**问题**（一期 B2/B3）：44 条伪造版本号 Android UA 逐请求轮换，同一
ttwid 在一条链上配过 46 个 UA；cookie-UA 配对完全随机。

**机制**：
- **UA 池真实化**：`internal/accounts/ua-pool.json` 21 条真实存在的桌面
  Chrome/Edge UA（Windows/macOS/Linux × 主版本 147-152），`MEDIAMON_UA_POOL`
  可换池文件；未接池时兜底确定性 Chrome/152。
- **会话级绑定**：引擎按会话（账号 | 平台默认身份）×代理 缓存会话客户端并
  抽一次 UA 固定——**同一 cookie 生命周期内 UA 不变，换号才换 UA**；账号
  自带 UA 优先。代码：`internal/collect/uabinding_test.go` 所测路径、
  `internal/httpclient/client.go`。
- **cookie jar**：`httpclient.Session()` 克隆客户端带独立 cookie jar——
  Set-Cookie 轮转（msToken 等）在一个身份生命周期内持久、跨身份隔离。

- 实测（二期）：单链 100 页内 UA 漂移 44 → **1**；整场 UA 数 53 → 11
  （= 身份数，而非请求数）。

### 原则 4：参数克制（count 钳制 + limit 语义 + 页数守卫）

**问题**（一期 C1/t02/t03）：`--limit 60` 直接透传成单页 `count=60`（真人
从不出现 60/100）；`--limit 0` 打满 100 页后**丢弃 ~1980 条已采数据**。

**机制**：
- **count 是页大小，不是 limit**：一律钳到 ≤20；`--limit 0` 时用契约
  `count_default`（兜底 20）；limit 只用于停页。配置：`MEDIAMON_MAX_COUNT`
  （默认 20）。
- **页数守卫可配置**：`MEDIAMON_MAX_PAGES`（默认 100）；触顶**保留已采
  数据 + 返回可续采游标**（计数 `collect.maxpages_hit`），CLI 侧
  flush-before-exit 先落盘再报错。
- 代码：`internal/collect/predicate.go`（maxCountPerRequest / maxPagesLimit
  / 翻页循环）、`internal/collect/defectfix_test.go`。

- 实测（二期）：count 值域 {20}（一期 {20,40,60,100}；真人 {20,50} 为主）；
  t03 maxPages 触顶 2000 条数据完整落盘（一期 0 条 + 报错）。

### 原则 5：退避纪律（重试 + 换号前退避 + Retry-After）

**问题**（一期 A2/E）：MaxRetries=0，注入 401 后 0-16ms 内 9 连发换号风暴。

**机制**：
- **传输层重试**：指数退避 `RetryBase·2^(n-1)` + ±20% 抖动（默认 base
  250ms，上限 30s），**尊重 Retry-After**（秒数/HTTP-date，钳 30s，取更大
  者）。配置：`MEDIAMON_MAX_RETRIES`（默认 2；`0`=显式恢复单次旧语义）。
  装配：mediactl / mediad / mediad-mcp 三处。代码：
  `internal/httpclient/client.go`（backoffFor / parseRetryAfter）。
- **换号前先退避**：`sleepBeforeRotation` = base·2^rotations·±25% 抖动，
  `MEDIAMON_ROTATE_BACKOFF_MS`（默认 1000；`0`=关闭）；观测
  `collect.rotation_backoff_total_ms`。代码：`internal/collect/rotate.go`。
- **封禁纪律**：连续 3 败自动封禁账号（保持一期语义不变）。

- 实测（二期）：注入 401 后时序 250/1078/2078/5172…（重试 250ms 起、
  换号 1s→2s→4s 指数 + 抖动）；一期为 0/16/16/281/422ms 连发。

### 原则 6：签名卫生（fail-closed，绝不发裸请求）

**问题/风险**：签名必需契约（dy a_bogus、xhs x-s/x-s-common 头签名）在
签名缺失/签名服务故障时若"照发不误"，等于主动送检。

**机制**：`signature.Required` 门在 buildURL 阶段 fail-closed——缺签名
参数/头、签名服务不可达、签名服务 5xx，一律**拒发（0 出网）**并报错；
`ReturnUnsigned=false` 实证（一期 FC1-FC4、FC6，二期复测 8/8 保持）。
开发环境配套：signsvc stub 为声明 `signature.headers` 的契约产占位头
（`cmd/signsvc`），不再喂不饱 xhs 类契约（一期 FC7 发现）。

### 原则 7：预算与封禁（页数/账号预算 + 健康探测三态）

**机制**：
- 页数预算见原则 4；数据永不因触顶而丢（partial + 游标）。
- **健康探测三态**（healthy / degraded / expired）：`internal/accounts/health.go`
  `ClassifyHealth`——401/403→expired；200+空绑定→expired（半死 cookie）；
  传输层/5xx→degraded；全链路通→healthy。探测与采集同源装配（签名/头集/
  UA 池/cookie），**探测流量本身也要静默**。
- **深度探测用真实游标**：`internal/collect/probe.go`——第二页探测用首页
  响应返回的 next_cursor（一期硬编码 "20" 对不透明 id 游标必空 → 好号被
  误判 expired）；无游标可取回退 `MEDIAMON_PROBE_DEPTH_CURSOR`（默认 "20"）。
- 账号轮换 maxRotations=2；CLI `--account auto` 已放行（与 mediad REST 一致）。

---

## 2. 实测数据支撑（一期 → 二期 → 真人基线）

复算脚本 `testlab/analyze_compare.py`（原始取证 `testlab/captures_round1/`
与 `testlab/captures/`，全部 127.0.0.1 源）。dy 主链 = t03 百页链（一期
102 页 / 二期 104 页）：

| 维度 | 一期（旧引擎） | 二期（本分支） | 真人语料基线 |
|---|---|---|---|
| dy 主链间隔 p50 | 562ms（一期报告口径 547ms） | **2062ms** | 页级翻页秒级 |
| dy 主链间隔 p90 | 609ms | **6500ms** | 1816ms（含 XHR 簇发） |
| dy 主链间隔 max | 2016ms（一期报告 657ms） | **27172ms** | 27019ms |
| dy 主链间隔 CV | 0.269（一期报告 6.6% 纯链） | **1.032** | 3.485 |
| 零间隔（≤5ms） | 1 | **0** | 9/337（XHR 簇发） |
| 均值头数 | 4.0-5.5 | **16.0-18.6** | 19.0-27.8 |
| Referer 在网率 | 0%（dy/ks） | **100%** | ≈100% |
| Accept-Language | 0% | **100%** | ≈100% |
| sec-ch-ua 族 | 0% | **100%（且与 UA 一致）** | 100%（xhs 语料 50%） |
| count/num 值域 | {20,40,60,100} | **{20}** | dy {20,50}；xhs {10,30} |
| 链内 UA 漂移（同 cookie） | **44**（100 页链） | **1**（同链恒定；跨进程测试 2-3） | 1（会话单 UA） |
| 401 后行为 | 0-16ms 内 9 连发 | **250ms/1s/2s/4s 指数退避 + 抖动** | 停顿/换页面 |

**残余差距（如实记录）**：①xhs 页大小固定 20，语料实证为 {10,30}——
契约 `count_default` 建议改 10 或 30（低cost，改契约 JSON 即可）；
②头数 16-18.6 vs 真人 19-27.8——缺 Accept-Encoding 四元组与部分站点头
（见 §3）；③TLS/h2 指纹仍是 Go 默认（见 §3）；④p50 2s 高于真人 p50
（真人 p50 低是 XHR 簇发所致；页级翻页语义下 2s 处于真人分布内，且 CV
1.03 已具备重尾形态）。

---

## 3. 高成本遗留（引 `Media-Monitor/docs/TODO-SILENT-HIGHCOST.md`）

| # | 项 | 可检测性 | 阻塞点 |
|---|---|---|---|
| 1 | **TLS/HTTP2 指纹拟态**（utls Chrome ClientHello + h2 帧序） | 高 | 违反 stdlib-only 依赖政策（go.mod 顶部声明），需先立 ADR；或改走浏览器农场代理 |
| 2 | **全局 QPS 治理**（按平台主机域令牌桶，多任务叠加限速） | 中 | engine/runner 层新增节流器；与页间节流互补（单链 vs 多链叠加） |
| 3 | **Accept-Encoding 四元组**（gzip, deflate, br, zstd） | 低-中 | stdlib 无 br/zstd reader，同样撞依赖政策 |
| 4 | **h2 头序/伪头** | 中 | 依赖 #1，随 utls 一起做 |

零真站约束下 TLS 面（JA3/h2 帧序）无法在本合成栈验证——合成站是
HTTP/1.1 明文。上线前应默认假设 TLS 指纹层仍可被平台识别，配合代理池/
浏览器农场兜底。

---

## 4. 部署建议（生产渐进启用）

1. **默认即静默**：本分支全部机制默认开启（pacing 1.5s/σ1.0、count≤20、
   MaxRetries=2、换号退避 1s、真实 UA 池、浏览器头集）。生产从默认起步，
   不要先关后开。
2. **灰度顺序**：①单任务低峰灰度（观察 `collect.page_sleep_total_ms`、
   `collect.rotation_backoff_total_ms`、`collect.maxpages_hit` 三个计数器）；
   ②按平台逐站放量；③多任务叠加前先落 QPS 令牌桶（§3-2），否则多链叠加
   会重新制造同源突发。
3. **容量预期**：页间 p50 1.5s 意味着单链吞吐 ≈ 20 条/2.5s ≈ 8 条/s 的
   数量级下降（一期匀速 20 条/0.6s）。任务编排需按新节奏重排预算：加并发
   任务数之前先加账号与出口 IP，**不要用调小 `MEDIAMON_PAGE_SLEEP_MS`
   换吞吐**。
4. **紧急模式**：`MEDIAMON_EMERGENCY=1` 仅限事故取证/时效任务，开启期间
   该实例流量特征回到一期水平，用完即关并记录使用时段。
5. **账号治理**：连续 3 败封禁 + 换号退避是账号池的止损底线；健康探测
   （三态）定期跑，把 expired 账号摘出轮换池，避免半死 cookie 反复撞墙。
6. **用本合成站回归**：任何采集器改动（尤其契约/头集/节流参数）上线前，
   在 oracle 合成栈全量回归：
   ```bash
   # 起栈（开发口）+ 台架服务
   bash oracle/up.sh --mode synth --synth-ports 8701,8702,8703 --no-855x-compat
   cd oracle/mediamonitor/testlab
   # proxy 8771-8773 → 8701-8703，fake_signer 8790，然后：
   D:/Projects/temp2/oracle/env/Scripts/python.exe build_adapt_synth.py
   bash run_p0.sh && bash run_failclosed.sh && bash run_p1.sh
   D:/Projects/temp2/oracle/env/Scripts/python.exe analyze_traffic.py    # 单轮矩阵
   D:/Projects/temp2/oracle/env/Scripts/python.exe analyze_compare.py    # 一期/二期/真人对照
   ```
   验收线：12/12 链路通、FC 全拒发、count 值域 ⊆ {≤20}、链内 UA 漂移=1、
   注入 401 后无 <200ms 连发。**全程零真站访问**（契约 base_url 全指
   127.0.0.1 记录代理）。

---

## 5. 红线清单（任何时候不许破）

1. 签名必需契约绝不发裸请求（fail-closed 是底线，不是优化）。
2. 不用调大 `MEDIAMON_MAX_COUNT` / 调小页间节流换吞吐——那是把检测面
   换成产能。
3. 不在契约里伪造平台没有的参数名（xhs 子评论主参数是
   `root_comment_id`，语料 64/64；dy 是 `comment_id`——参数名以语料裁决
   为准，写进契约 placeholder，不让引擎硬编码）。
4. 错误后第一反应是退避，不是换号连发；换号前先退避。
5. 触顶/出错时已采数据必须落盘（partial + 游标），数据损失不可逆而
   节奏损失可逆。
