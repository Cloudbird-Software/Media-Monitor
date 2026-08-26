# LICENSE-PROTOCOL — 许可证文件与验签协议

本文档定义 Media-Monitor 客户端的许可证（license）格式、机器指纹算法、
Ed25519 验签流程与在线校验接口约定。实现见 `internal/license`。

**签发服务端不在本仓**。本仓只包含验证端（fail-closed）与一个仅供签发端/
测试使用的 `Sign`/`GenerateKey` 辅助函数；私钥不进本仓。

## 1. License 文件

- 位置：`<数据目录>/license.json`（`license.LicenseFileName`）。
- 编码：UTF-8 JSON，单文档。
- 字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `machine` | string | 绑定的机器指纹（见 §2），逐字符相等比较 |
| `not_before` | int64 | 生效时间，Unix 秒 |
| `not_after` | int64 | 过期时间，Unix 秒（含端点） |
| `features` | []string | 启用的功能标志（如 `collect`、`dm`、`live`） |
| `issuer` | string, 可选 | 签发方标识，仅展示用 |
| `signature` | string | base64（StdEncoding）编码的 Ed25519 签名，64 字节 |

### 签名覆盖范围（canonical payload）

签名只覆盖以下字段、按固定顺序的 JSON 序列化（Go `encoding/json` 结构体
字段序，无多余空白）：

```json
{"machine":"...","not_before":0,"not_after":0,"features":["..."],"issuer":"..."}
```

- `features` 为 `null`/空数组按 Go 零值序列化；签发端与验证端用同一结构
  体（`payloadBytes`）生成 payload，保证字节级一致。
- `signature` 自身不参与签名；JSON 中的未知字段被忽略且不被签名覆盖
  （不得在其中放置安全相关语义）。

## 2. 机器指纹（Windows）

`license.MachineFingerprint()`，stdlib-only（`os/exec`，无 x/sys）：

1. 主路径：`reg query HKLM\SOFTWARE\Microsoft\Cryptography /v MachineGuid`，
   解析输出中 `MachineGuid` 行的第 3 个字段（REG_SZ 值）。
2. 回退 1：`wmic csproduct get UUID`，取首个非表头非空行。
3. 回退 2：两者都失败时返回 `"unknown-" + sha1(hostname)` 的 hex，保证
   确定性但不保证唯一性——签发端应拒绝为 unknown 指纹签发。

指纹原样写入 license 的 `machine` 字段；验证端逐字符比较，不匹配即拒绝
（`ErrMachineMismatch`）。

## 3. 验证流程（离线，fail-closed）

`Verifier.Verify(lic)` 按序执行，任一步失败即拒绝：

1. 用 §1 的 canonical payload 规则重建待签字节。
2. base64 解码 `signature`（解码失败=拒绝）。
3. `ed25519.Verify(内置公钥, payload, signature)`，失败返回
   `ErrInvalidSignature`。公钥随二进制内置（32 字节）。
4. 时间窗：`not_before <= now <= not_after`，否则 `ErrNotActive`。
5. 机器指纹匹配（§2），否则 `ErrMachineMismatch`。
6. 若接线了 `OnlineVerifier`，执行在线校验（§4），失败返回
   `ErrOnlineFailed`。

全部通过才算有效 license。

### Gate（cmd 层门禁）

`license.LoadGate(dataDir, pub, online)` 读取 license.json 并验证；
`Gate.Check(feature)` 供采集/动作类入口调用：

- 返回 `nil` = 放行。
- 返回 `*DeniedError`（`Reason` 为稳定枚举：`no_license` / `malformed` /
  `bad_signature` / `machine_mismatch` / `expired_or_inactive` /
  `online_failed` / `feature_disabled`）= 拒绝。

上层映射约定：**无 license 或验证失败时，拒绝采集/动作类接口，放行面板
（dashboard）与 version 接口**。面板与 version 不得调用 Gate。

## 4. OnlineVerifier 接口约定（可选在线校验）

```go
type OnlineVerifier interface {
    Verify(machine string, lic License) (bool, error)
}
```

- 由宿主（cmd 层）把其许可证服务客户端接进来；`nil` 表示只做离线校验。
- 语义：`(true, nil)` 放行；`(false, nil)` 或 `(_, err != nil)` 均拒绝
  （在线校验 fail-closed，网络故障不放行）。
- 调用时机：离线校验全部通过之后；传入的 `lic` 已完成签名校验
  （`RawPayload` 已填充）。

## 5. 签发端职责（服务端，不在本仓）

1. 持有 Ed25519 私钥；公钥交付给客户端内置。
2. 采集购买者机器指纹，生成 license 字段，按 §1 canonical payload 规则
   序列化后 `ed25519.Sign`，base64 编码写入 `signature`。
3. 合理设置 `not_before`/`not_after` 与 `features`；换机需重新签发
   （指纹绑定）。
4. 在线校验服务实现 §4 语义（吊销/封禁以 `(false, nil)` 表达）。
5. 本仓的 `Sign`/`GenerateKey` 与签发算法一致，可用于签发端参考实现
   与测试夹具，但私钥与签发服务代码不进入本仓。
