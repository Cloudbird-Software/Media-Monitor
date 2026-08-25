# AGENTS.md（索引型——只放不可推断的约束，宪法 §4D ≤30 行；细节按需读索引）

<!-- entry-protocol v1 -->

### 入口协议（陌生 agent 从这里开始——宪法 §11 / ADR-0055）

1. 取 ghcb（钉 SHA，禁浮动 main）：`curl -fsS -o ghcb https://raw.githubusercontent.com/Cloudbird-Software/.github/f72d9520706c8fca974d92456f65cae5c1412bb7/scripts/ghcb && chmod +x ghcb`（凭据用你自己的：`gh auth login` 或 `export GH_TOKEN=<PAT>`；`-f` 必带——404 时 curl 无 -f 仍退出 0，会把错误页当脚本落盘）
2. 找活：`bash ghcb next [owner/repo]` → 列 state:ready 卡（卡 issue 是唯一工作凭证，无卡不开工）
3. 认领：`bash ghcb claim <n> [owner/repo]` → 评论 /claim——conductor 转介 arbiter 原子 CAS 租约，先到先得；败者换下一张（`bash ghcb status <n>` 看持有者）
4. 开工：`make card-test CARD=<n>`（读卡 AC、测试先行）→ `make gates-pr`（本地复现 CI 关卡）
5. 提 PR：body 必带一行卡元数据 `Card: <owner>/<repo>#<n>`（`bash ghcb card-meta <n>` 生成；缺失=后续关卡 exit 3）
6. front-desk 命令（卡 issue 评论，conductor 转介 arbiter 处理）：/claim 认领 · /release 释放租约 · /retry 隔离回流

<!-- /entry-protocol -->

## 硬规则（违反 = PR 打回）

1. 认证：一切 push/PR 用 cloudbrid-agent App 令牌，禁个人 PAT。获取（脚本 pin 到已审阅提交，升级先比对 .github main 再换 SHA——禁 `curl|bash` 浮动 main 指针，ADR-0021）：
   `GH_TOKEN=$(REPO=template-service bash <(curl -sS https://raw.githubusercontent.com/Cloudbird-Software/.github/487fd930c46a86bf3fb6865a7223287f8e3446e2/scripts/gh-app-token.sh))`
2. 不改 `.github/workflows/**`、`Makefile` 的 check 目标（App 无此权限，人类专属）
3. 新依赖先报"名称/用途/许可证/标准库可否替代"等人批；禁 AGPL/GPL-3.0/SSPL；密钥/客户名/连接串不进仓库（`.env.example` 占位）
4. 一个 PR 一件事，diff < 400 行；bug 修复先写复现失败测试；对外接口变更写 CHANGELOG.md；提交信息 Conventional Commits

## 索引（用到再读，不要全读）

- 本地命令：`make setup` 安装 / `make check` 提交前必跑（lint+arch+test）/ `make test <文件>` 单测；建模块/动边界 → [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- 选语言/选库 → [.github 仓 governance/policy/languages.yaml](https://github.com/Cloudbird-Software/.github/blob/main/governance/policy/languages.yaml)；测试政策 → [testing.yaml](https://github.com/Cloudbird-Software/.github/blob/main/governance/policy/testing.yaml)
- 治理措施总清单 → [.github 仓 governance/GOVERNANCE.yaml](https://github.com/Cloudbird-Software/.github/blob/main/governance/GOVERNANCE.yaml)；模块内工作 → 该模块目录下的 AGENTS.md
