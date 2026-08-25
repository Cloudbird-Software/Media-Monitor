# template-service

Cloudbird Software 新项目模板。用 GitHub "Use this template" 或
`gh repo create Cloudbird-Software/<name> --template Cloudbird-Software/template-service` 创建新仓库，自动继承全部护栏。

## Makefile 接口（所有语言统一，CI 只认这个）

| 目标         | 作用                                     |
| ------------ | ---------------------------------------- |
| `make setup` | 安装依赖（`npm ci`）                     |
| `make fmt`   | 格式化                                   |
| `make lint`  | prettier --check + eslint + tsc --noEmit |
| `make test`  | vitest + coverage                        |
| `make build` | 构建                                     |
| `make check` | lint + test，**提交前必须全绿**          |

## CI 结构

- `hygiene`：密钥扫描（gitleaks）、大文件/凭据文件拦截、zizmor Actions 审计
- `check`：`make setup && make check`
- `deps`：依赖漏洞 + 许可证审查（PR 时）
- `gate`：聚合门（组织 ruleset 的唯一必需 check）

工作流实现在 [CI-Workflows](https://github.com/Cloudbird-Software/CI-Workflows)，本仓只引用 `@v1`。
