/** dependency-cruiser 配置 —— 架构边界守卫
 *
 * 作用：防止模块边界腐化（"写得散"）。规则随项目演进填充，
 * 骨架保证机制在每个新仓开箱即用。
 *
 * 新仓第一个任务：让 AI 根据实际模块结构把 TODO 规则补全。
 * 文档: https://github.com/sverweij/dependency-cruiser
 */
module.exports = {
  forbidden: [
    {
      name: "no-circular",
      severity: "error",
      comment: "循环依赖 = 边界失败，必须拆模块",
      from: {},
      to: { circular: true },
    },
    {
      name: "no-orphans",
      severity: "warn",
      comment: "孤立文件通常是残留死代码",
      from: { orphan: true, pathNot: ["^src/index\\.ts$"] },
      to: {},
    },
    // TODO: 项目首个 PR 时由 AI 根据模块地图补全，示例：
    // {
    //   name: "domain-not-import-infra",
    //   severity: "error",
    //   comment: "领域层不得依赖基础设施层",
    //   from: { path: "^src/domain/" },
    //   to: { path: "^src/infrastructure/" },
    // },
    // {
    //   name: "entry-only-imports",
    //   severity: "error",
    //   comment: "跨模块只能 import 入口文件（index.ts），不得深入实现",
    //   from: { pathNot: ["^src/([^/]+)/index\\.ts$", "^src/index\\.ts$"] },
    //   to: { path: "^src/([^/]+)/(.+)$", pathNot: ["^src/$1/index\\.ts$"] },
    // },
  ],
  options: {
    doNotFollow: { path: "node_modules" },
    tsConfig: { fileName: "tsconfig.json" },
    reporterOptions: { dot: { theme: { graph: { rankdir: "TB" } } } },
  },
};
