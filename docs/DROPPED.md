# 边缘功能清点 · 原软件占位条目与放弃判据

> 本文件记录原软件（baokeai）边缘功能的清点结论：哪些是原软件真实功能
> （已实现），哪些在原软件中就是占位（放弃合理）。
> 2026-08-26 返工重写：此前版本误判视频号/微信多开/内容加密"无实现证据"，
> 经独立审计推翻——三者均为原软件真实功能，已实现。判据说明见文末。

## 判据说明

本次清点以 `app-extracted/public/dist/js/app.js` 路由表为**一手判据**：

- 路由的 `component` 指向真实 vue 视图 → 真实功能；指向占位三件套
  （`dev/deving.vue`、`dev/devmobile.vue`、`dev/launched.vue`）→ 占位。
- 路由带 `isDev: true` 标记 → 占位/未上线；`isDev` 被注释掉视为上线态。
- 主进程字符串表（`_strings.js`）只含 `controller.example.*` 完整名
  （随包脚手架），真实功能的 IPC 走 `service.*` 事件 + 原子串拼接，
  故"字符串表搜不到 controller.x"**不能**作为无实现证据。此前版本
  正是误用了这一判据。

## 原软件真实功能 · 已实现

### 视频号留痕群控

- 证据：`app-extracted/public/dist/js/17.js` = `views/shipinhao/liuHen.vue`
  （"视频号留痕控制端"）；app.js 路由 `shipinhaoqunkong` 无 `isDev` 标记；
  主进程有 `cc.oxcc.sph` 包名与 scrcpy 运行时（手机群控执行链路）。
- 实现：`internal/trace` 引擎 + `adapt/flows/shipinhao-trace.json` 策略
  （动作概率照 17.js setting 默认值：like 0.8 / follow 0.2 / collect 0.8 /
  dm 0.1 / comment 0.1，主页停留与作品浏览均 4–8 秒）。
- 保守占位：`profile_url_template` 为 `weixin://finder/profile/{sec_uid}`
  占位（17.js 中 `persionHomeUrl` 恒空，主页打开在手机端群控 App 内）；
  flow JSON 的 `profile_url_template_comment` 已注明，接线前需真机校正。
- 例外：视频号"用户搜索""评论自动回复"两个子项在原软件即占位
  （`dev/deving.vue`，`isDev: true`）→ 放弃合理，见下节。

### 微信多开

- 证据：`baokeai/resources/extraResources/more/openwechat.exe` 物理存在；
  `app-extracted/public/dist/js/5.js` = `views/utils/mWechat.vue`；
  路由 `isDev` 被注释掉属上线态。
- 实现：`internal/toolbox/wechat` + `mediactl toolbox wechat-multi`
  （helper 路径可配，缺失 fail-closed）。

### 内容加密工具

- 证据：`app-extracted/public/dist/js/27.js` = `views/utils/secContent.vue`；
  零宽字符 U+200C（`ZW_CHAR`）隐写 + 手机号风格映射；纯渲染进程实现，
  故无 IPC——这是此前"搜不到 controller"误判的根因。
- 实现：`internal/toolbox/encrypt` + `mediactl toolbox encrypt/stylize`
  （行为逐行移植：ZW_CHAR 随机长度 [min,max] 默认 10..30；手机号映射表
  逐字移植）。

## 原软件即占位 · 放弃合理

### 视频号"用户搜索"与"评论自动回复"

- 证据：app.js 路由 component 指向 `dev/deving.vue`，`isDev: true`。
- 结论：原软件即占位 → 放弃合理。

### AI 仿写

- 证据：app.js 路由 component 指向 `dev/launched.vue`，`isDev: true`，
  页面文案"联系客服开通"；字符串表中的 `AiDataInfo` 实为 protobuf 频控
  消息名，与仿写无关。
- 结论：原软件即占位 → 放弃合理。

### 加微助手

- 证据：app.js 路由 component 指向 `dev/devmobile.vue`，`isDev: true`。
- 结论：原软件即占位 → 放弃合理。

### 聚聊天

- 证据：app.js 路由 `juliaotian` 指向 `mWechat.vue` 且 `isDev: true`
  （占位入口）；`juliaotian.com` 为品牌更新服务器域名，非功能实现。
- 结论：原软件即占位 → 放弃合理。

### 电商/外卖/地图线索

- 证据：UI 全部占位（路由指向 `dev/deving.vue` / 菜单注释）。主进程
  字符串表有 `goods` 表 CRUD SQL 与 `babycopy`/`tao1688` 字符串，但均属
  随包发行的 `controller.example.*` 脚手架残留（Electron 示例控制器），
  非产品功能。
- 结论：定性为**脚手架残留** → 放弃合理。

## 附：有实现证据、此前已实现的条目

### 网络抓包工具（netcapture）

- 证据：chunk 13（UI）+ `controller.netcapture.*` IPC
  （getSession/openRecorder/setRecording）+ `service.netcapture.record` 事件。
- 实现：`internal/netcapture`（核心包）+ `mediactl netcapture` CLI
  （会话查询 + HAR 导出；CLI 无头环境不做 CDP 录制）。
