# TODO — 静默化高成本项（本分支未做）

> 一期报告待办中可检测性高但修复成本高/需架构决策的项。本分支（feature/silent-scraping）
> 只完成 Top5+低成本项；以下留待后续决策。零真站约束下无法验证 TLS 面的项也在其中。

## 1. TLS / HTTP2 指纹拟态（待办#6，可检测性高 / 成本高）

- 现状：Go stdlib `net/http` 的 ClientHello（JA3=Go 默认）、h2 帧序/SETTINGS 均为 Go 默认；
  HTTP/1.1 头序为 Go 排序写出（一期 §4-F）。平台 TLS 层一层即可识别非浏览器客户端。
- 做法（报告建议）：utls Chrome ClientHello + h2 帧序模拟，或经浏览器农场代理出流量。
- 阻塞：引入第三方依赖违反本仓库 stdlib-only 依赖政策（go.mod 顶部声明，C1 供应链变更
  需组织级审批，见 docs/OPERATIONS.md）——**需先立 ADR**；浏览器农场方案则是部署面改造。
- 落点：`internal/httpclient/client.go`（Transport 构造处）。

## 2. 全局 QPS 治理（待办#7，可检测性中 / 成本中）

- 现状：引擎并发安全但无全局节流器；多任务叠加时（一期 §4-D：4 并行 collect 落点
  0/7.2/7.8/7.8s）服务端一快即真并发，平台侧可见同源突发。
- 做法：按平台主机域的令牌桶限速器，装配在 engine/runner 层，多任务共享。
- 注意：与页间节流（本分支已做）互补——节流管单链节奏，QPS 管多链叠加。
- 落点：`internal/collect/engine.go` clientFor/fetchClient 之前或 `internal/core` runner。

## 3. Accept-Encoding 完整四元组（B4 部分，成本中）

- 现状：浏览器头集携带 `Accept-Encoding: gzip, deflate`（Go 手工设置该头后不透明解压，
  服务端若真选 deflate 会拿到压缩体）。真人 Chrome 发 `gzip, deflate, br, zstd`。
- 做法：补 br/zstd 需要响应解压支持（stdlib 无 br/zstd reader，又回到第三方依赖/
  ADR 问题），或维持 gzip-first 策略（多数 CDN 首选 gzip，风险低）。
- 落点：`internal/platforms/*/browserhdr.go` + `internal/httpclient`（响应解压）。

## 4. HTTP/2 头序/伪头（B4，成本高）

- 现状：Go h2 会按自身规范小写化并排序部分头；真人 Chrome 有固定头序
  （:authority/:method/:path/:scheme + accept 等 ~20 头，一期取证 header_order）。
- 做法：依赖 #1 的 h2 拟态（transport 层控制帧与头块顺序），单独立项无意义，随 #1 一起。

## 5. xhs 子评论参数名默认值对齐（待 A 线结论）

- 本分支已做配置化（`transport.reply_target_param`，见 CHANGELOG-SILENT §6-4）。
- **TODO-C线**：A 线语料裁决（comment_id vs root_comment_id）结论出来后，
  在适配契约（adapt/contracts/xhs-comments-replies.json 或 testlab 适配层）设置该值，
  并与合成站（oracle/replay/synth_api.py）对齐复测 t10。
