# CHANGELOG-SILENT — 采集器静默化二期（feature/silent-scraping）

依据 `D:/Projects/temp2/oracle/mediamonitor/test_report_round1.md` 改进待办 Top5 + 附带低成本项，改造 Media-Monitor 本体。
全程零真站访问（编译/单测/loopback mock 验证）；分布参数与头集形态取自 `oracle/recording` 语料（只读参考，未复制任何敏感值）。

## 1. 页间节流 + 对数正态抖动（待办#1 / A1）

翻页循环内注入对数正态"思考时间"，替代纯服务端时延回声（一期实测 p50=547ms、CV=6.6% 的匀速巡航曲线）。

- 分布：lognormal(median=1500ms, σ=1.0)，钳制 [250ms, 30s]
  → p50=1.5s、p90≈5.4s、p99≈15s（对标真人重尾：dy max 2.2s / xhs 6.1s / ks 28s）
- **默认开启**
- 配置方式：
  - 全局：`MEDIAMON_PAGE_SLEEP_MS`（中位数毫秒；`0` = 关闭）
  - 全局：`MEDIAMON_PAGE_SLEEP_SIGMA`（对数空间 σ，默认 1.0）
  - **紧急模式**：`MEDIAMON_EMERGENCY=1`（任何非空非 0 值）一键关闭节流
  - 契约级：`paging.page_sleep_ms`（正数=覆盖中位数；`-1`=仅该契约关闭）
- 观测：`collect.page_sleep_total_ms` 累计毫秒数

## 2. 三站浏览器级头集 + cookie jar 会话绑定（待办#2 / B1+B3）

一期引擎只发 4-5 个头（Referer 0%、Accept-Language 0%、sec-ch-ua 0%）；真人基线 19-28 头。

- 新增 `internal/platforms/{douyin,kuaishou,xhs}/browserhdr.go`：
  Accept / Accept-Language / Accept-Encoding / Referer / Origin / Sec-Fetch 族 / Priority / Cache-Control / Pragma（形态取自语料 XHR 请求；**不含 cookie 等敏感值**）
- 引擎合并优先级：平台默认头 < 签名头（x-s/x-s-common）< 契约 `transport.headers`（可逐项覆盖）< UA 一致 sec-ch-ua 族 < Cookie/UA
- `sec-ch-ua` / `sec-ch-ua-mobile` / `sec-ch-ua-platform` 由**所发 UA 实时派生**（版本/品牌/平台一致）
- **会话绑定**：`httpclient.Session()` 克隆客户端带独立 cookie jar；引擎按（账号 | 平台默认身份）×代理 缓存会话客户端——Set-Cookie 轮转（msToken 等）在一个 cookie 生命周期内持久、跨身份隔离；轮换克隆共享会话缓存
- 装配：mediactl（collect/probe）、mediad、mediad-mcp 四处注入 `BrowserHeaders`

## 3. count 钳制（待办#3 / C1）

一期 `--limit 60` 直接透传成单页 `count=60`（真人从不出现 60/100）。

- **count 是页大小，不是 limit**：一律钳到 ≤20；`--limit 0` 时用契约 `count_default`（兜底 20）
- limit 只用于停页；`--limit 0` = 无记录上限，安全页大小走到自然终点或页数守卫
- 默认上限 20，可配置：`MEDIAMON_MAX_COUNT`（如 50）
- 页数守卫可配置：`MEDIAMON_MAX_PAGES`（默认 100）

## 4. UA 池真实化 + 会话绑定（待办#4 / B2+B3）

一期 44 条池全是伪造版本号 Android UA（Chrome/138.2.8.5 乱码版本、仅两种机型），且逐请求轮换（同一 ttwid 配 46 个 UA）。

- `internal/accounts/ua-pool.json` 重建：**21 条真实存在的桌面 Chrome/Edge UA**（Windows/macOS/Linux × 主版本 147-152，对齐语料真人基线 Chrome/152）；`httpclient` 默认池同步替换
- **会话绑定**：引擎按会话（账号或平台默认身份）从池中抽一次并固定——同一 cookie 生命周期 UA 不变，**换号才换 UA**；账号自带 UA 优先；未接池兜底确定性真实 Chrome/152
- 路径覆盖：`MEDIAMON_UA_POOL`（现有机制）

## 5. 退避与重试（待办#5 / A2+E）

一期 MaxRetries=0 实证（429/5xx 单次尝试即败）；错误后 0-16ms 连发。

- httpclient：指数退避 `RetryBase·2^(n-1)` + ±20% 抖动（默认 base 250ms，上限 30s），**尊重 Retry-After**（秒数/HTTP-date，钳 30s，取更大者）
- 装配：mediactl / mediad / mediad-mcp 全部接入 `MaxRetriesFromEnv`
  - `MEDIAMON_MAX_RETRIES`（默认 **2**；`0` = 显式恢复单次旧语义）
- **换号前先退避**：`sleepBeforeRotation` = base·2^rotations·±25% 抖动
  - `MEDIAMON_ROTATE_BACKOFF_MS`（默认 1000；`0` = 关闭）；3 连败封禁逻辑保持不变
- 观测：`collect.rotation_backoff_total_ms`

## 6. 缺陷修复（一期 4 个）

1. **`--limit 0` 打满 maxPages 且报错丢弃 ~1980 条已采数据**：
   - limit 未设/0 → 安全默认页大小（见 #3）；maxPages 触顶**保留已采数据 + 返回可续采游标**（不再 nil+err），计数 `collect.maxpages_hit`
   - 错误路径贯通：引擎六个翻页入口出错时返回 partial 数据+err；CLI search/comments/replies/group **先落盘（emitAll flush-before-exit）再报错**
2. **probeEngine 漏注 Signers**（dy 探测永不出网误判 expired）：与 collectEngine 同源装配 signers（`MEDIAMON_SIGNER_URL/TOKEN`），并补 BrowserHeaders/UAPool
3. **probe 深度游标硬编码 "20"**（xhs 不透明 id 游标必空 → 好号误判 expired）：改用**首页响应返回的真实 next_cursor** 做第二页探测；`has_more=false` 不再深探；无游标可取时回退 `MEDIAMON_PROBE_DEPTH_CURSOR`（默认 "20"）
4. **xhs 子评论参数名**（comment_id vs root_comment_id）：契约可声明 `transport.reply_target_param`，引擎按该名传递顶层评论 id；未声明保持默认 comment_id。
   **TODO-C线**：默认值等 A 线语料裁决结论，由 C 线在适配契约中设置该值并对齐合成站（代码内已留 TODO-C线 标注）。

## 7. 附带低成本项（待办#10/#11）

- CLI `--account auto` 放行（原被 accountPoolFor 拦 "not found"，auto 仅 mediad REST 可用）
- 游标哨兵显式识别 `"no_more"`（不再依赖数值串 asBool 巧合止链）
- signsvc stub 为声明 `signature.headers` 的契约产占位头（FC7：开发环境喂不饱 xhs 类契约）

## 配置速查

| 环境变量 | 默认 | 含义 |
|---|---|---|
| `MEDIAMON_PAGE_SLEEP_MS` | 1500 | 页间思考时间中位数（0=关闭） |
| `MEDIAMON_PAGE_SLEEP_SIGMA` | 1.0 | 对数正态 σ（重尾） |
| `MEDIAMON_EMERGENCY` | 空 | 非空=紧急模式，关闭节流 |
| `MEDIAMON_MAX_COUNT` | 20 | 单请求 count 硬上限 |
| `MEDIAMON_MAX_PAGES` | 100 | 翻页页数守卫 |
| `MEDIAMON_MAX_RETRIES` | 2 | 429/5xx 重试次数（0=单次） |
| `MEDIAMON_ROTATE_BACKOFF_MS` | 1000 | 换号前退避 base（0=关闭） |
| `MEDIAMON_PROBE_DEPTH_CURSOR` | "20" | 深度探测回退游标 |
| `MEDIAMON_UA_POOL` | `<exe>/data/ua-pool.json` | UA 池文件路径（缺省用内嵌 21 条真实池） |

## 验证产物

- 单测：`internal/collect/{pacing,browserhdr,countclamp,uabinding,defectfix,rotatebackoff}_test.go`、`internal/httpclient/backoff_test.go`
  （fake clock=sleepHook / seeded RNG / httptest fake transport）
- loopback mock 链验证：`quality/silent-mockchain-report.md`
  （`SILENT_MOCKCHAIN_REPORT` 环境变量可重定向输出路径）

## 未做（高成本，见 docs/TODO-SILENT-HIGHCOST.md）

- TLS/HTTP2 拟态（utls，违 stdlib-only，需 ADR）
- 全局 QPS 治理（按主机域令牌桶）
- Accept-Encoding 完整四元组（gzip, deflate, br, zstd）——需配套响应解压支持
