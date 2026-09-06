# ADR-0100: TLS/HTTP2 指纹拟态传输层（utls/fhttp 例外授权）

状态：已接受（用户批示 2026-09-06「实现指纹层的真人不可区分。必要的话，参考开源项目实现，乃至直接用开源工具」）
日期：2026-09-06

## 背景

真站原子能力测试（oracle/mediamonitor/real_live_capability_test.md）证明：MM 的
stdlib-only HTTP 栈在**应用层**（签名/参数/头集/节流）静默化达标后，仍存在与真浏览器
可区分的**传输层指纹**——TLS ClientHello（JA3/JA4）与 HTTP/2 设置帧（Akamai 指纹）。
Go 标准库的握手特征与 Chrome 不同，属「签名代际升级」之外的最后一块检测面
（docs/TODO-SILENT-HIGHCOST 长期项，用户现已批示解除挂起）。

## 决策

1. 引入 `github.com/bogdanfinn/tls-client`（MIT）+ 其传递依赖 `bogdanfinn/fhttp`、
   `refraction-networking/utls`（BSD-3），作为**可选传输模式**接入
   `internal/httpclient`——**打破 stdlib-only 约定**（首次例外，本 ADR 即授权记录）。
2. 指纹档：`profiles.Chrome_152`，与账号池钉死 UA（Chrome/152.0.0.0）严格同代。
3. 开关：环境变量 `MEDIAMON_TLS_IMPERSONATE=chrome152`（默认空=stdlib 原行为，
   全部既有测试零影响）。启用时 `Client.Do` 的重试/退避/Retry-After 语义保持不变。
4. Cookie 语义：引擎的会话 Cookie 由请求头显式携带（账号驱动）；拟态客户端不另设
   cookie jar，避免双源冲突。响应 Set-Cookie 沿用既有处理路径。
5. 验证口径：`tls.peet.ws/api/all` 对比 stdlib / 拟态 / 真 Chrome 三方 JA3/JA4 与
   H2 指纹；拟态档必须与 Chrome_152 基线一致。

## 后果

- 正面：传输层与真 Chrome 不可区分（检测面收敛到「无」——平台侧残余感知仅剩
  应用层签名与频率/节奏，均已由静默化方法学覆盖）。
- 负面：go.mod 新增 4 个依赖；stdlib-only 承诺正式修订（AGENTS.md 需同步注记，
  后续 PR 携带）；fhttp 与 net/http 类型不通用，拟态路径需独立的请求构造代码
  （impersonate.go，约束在 httpclient 包内，不外溢）。
- 中性：合成站不受影响（loopback HTTP 无 TLS 面）。
