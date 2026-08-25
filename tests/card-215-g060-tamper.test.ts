// W2-C2 验收测试（.github#215 / ADR-0061，AC-2 锁定集篡改契约）
// 本文件是 AC-1 的 test-first 演示体：单独先于实现 commit 入库（红），实现落地后转绿——
// git 历史与 CI 记录可证（quality/gates/g050-fail-before.sh 机器判定"红必须是断言失败"）。
// 命名约定 tests/card-<卡号>-*.test.ts = 卡级测试集（make card-test CARD=215 选中；g160 前的临时绑定）。
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { describe, expect, it } from "vitest";

const execFileAsync = promisify(execFile);
const repoRoot = join(fileURLToPath(import.meta.url), "..", "..");
const gate = join(repoRoot, "quality", "gates", "g060-test-tamper.sh");
const LOCKED_CONTENT =
  'import { it } from "vitest";\nit("locked", () => {});\n';

const sha256 = (s: string): string =>
  createHash("sha256").update(s, "utf8").digest("hex");

/** 造最小锁定集 fixture 并跑真实 g060；spawn 异常折叠为 99——红必须是断言失败，不许抛运行时错 */
async function runGate(lockedSha: string, current: string): Promise<number> {
  const dir = mkdtempSync(join(tmpdir(), "w2c2-g060-"));
  mkdirSync(join(dir, "tests"));
  writeFileSync(join(dir, "tests", "locked.test.ts"), current, "utf8");
  const manifest = {
    version: 1,
    specVersion: 1,
    entries: { "tests/locked.test.ts": { sha256: lockedSha, card: "fixture" } },
  };
  writeFileSync(
    join(dir, "baseline.json"),
    `${JSON.stringify(manifest, null, 2)}\n`,
    "utf8",
  );
  try {
    await execFileAsync("bash", [gate], {
      cwd: dir,
      env: {
        ...process.env,
        GATE_LOCK_FILE: join(dir, "baseline.json"),
        GATE_CHANGED_FILES: "tests/locked.test.ts",
        GATE_PR_AUTHOR: "stranger-agent", // 非 owner：篡改判定路径
      },
    });
    return 0;
  } catch (err) {
    const code = (err as { code?: unknown }).code;
    return typeof code === "number" ? code : 99;
  }
}

// 每用例 30s：g060 经 contract.sh 读契约 + schema 自校验，多次拉起 python/node
// 子进程——Windows 本地单次 3-6s（CI Linux 数十 ms），vitest 默认 5s 会误杀。
const T = 30_000;

describe("g060-test-tamper 退出码契约（W2-C2 锁定集）", () => {
  it(
    "非 owner PR 改动锁定路径 → exit 2（fail-escalate，只人类可解）",
    async () => {
      expect(await runGate("0".repeat(64), LOCKED_CONTENT)).toBe(2);
    },
    T,
  );

  it(
    "锁定集完好（sha 一致）→ exit 0",
    async () => {
      expect(await runGate(sha256(LOCKED_CONTENT), LOCKED_CONTENT)).toBe(0);
    },
    T,
  );
});
