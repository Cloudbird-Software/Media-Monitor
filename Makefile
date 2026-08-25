.PHONY: setup fmt lint arch test build check card-test gates-pr gates-fast
setup:  ; npm ci
fmt:    ; npx prettier --write .
lint:   ; npx prettier --check . && npx eslint . && npx tsc --noEmit
arch:   ; npx depcruise src
test:   ; npx vitest run --coverage
build:  ; npm run build

# ---------- quality 关卡（W2-C1 / ADR-0060；宪法 §4E 验证器之验证） ----------
# gates-fast：quality 关卡自测（零网络/零 LLM，bash+python 标准库）——关卡自身
# 变更必须带测试。check 串上它：lint 逻辑坏=整仓红，不留"检查器无人检查"的洞。
# （check 目标属人类专属治理面（AGENTS.md 硬规则 2），本次经卡 #214/#215 blastRadius
#  授权改动，合并由人把关。）
# W2-C2（.github#215 / ADR-0061 决策 5）升级为统一编排器入口：gates-fast =
# run-gates fast（quality 脚本语法 + 契约解析冒烟 + 全部自测：run-all + test-topology）。
gates-fast: ## 本地快跑：quality 自测 + 脚本语法 + 契约解析（W2-C1 #214 + W2-C2 #215）
	@bash quality/run-gates.sh fast

check:  lint arch test gates-fast

# ---------- 入口协议块第 4 步（W1-C3 / ADR-0055；W2-C2 升级编排器） ----------
# card-test：拉卡 AC 列表提示测试先行 + 卡级测试集编排（run-gates card——选中
# tests/card-<N>-*.test.ts，g160 AC 绑定前的临时绑定）。
CARD ?=
REPO ?= Cloudbird-Software/.github   # 卡所在仓（W1 波次卡都在治理仓；产品仓自有卡时 REPO=<owner>/<repo> 覆盖）

card-test: ## 读卡 AC 列表并提示测试先行：make card-test CARD=<issue#>
	@test -n "$(CARD)" || { echo "用法: make card-test CARD=<issue#>（缺 CARD）" >&2; exit 2; }
	@echo "== 卡 $(REPO)#$(CARD) 的 AC（测试先行：先按 AC 写红测试再实现）=="
	@gh issue view "$(CARD)" -R "$(REPO)" --json number,title,body \
	  --jq '"#\(.number) \(.title)\n\n\(.body)"' 2>/dev/null \
	  | awk 'NR==1{print;print ""} /^## AC/{f=1} f{print} f && /^## / && !/^## AC/{exit}' | head -60
	@echo "(空=拉取失败或卡无 AC 节——手动: gh issue view $(CARD) -R $(REPO))"
	@bash quality/run-gates.sh card $(CARD)

# ---------- W2-C2 测试产物拓扑三命令（.github#215 / ADR-0061 决策 5）----------
# 三命令与 CI（ci.yml quality-gates job）同一编排器入口 quality/run-gates.sh——
# 同一脚本、同一 GATE_* env 注入协议，本地与 CI 结果一致（AC-4）。
gates-pr: ## 本地复现 CI 关卡等价物（quality 关卡 + node 检查面；W2-C2 ADR-0061）
	@bash quality/run-gates.sh pr
	@echo "== 开 PR 前检查单（机器不可判部分）：PR body 引用 ADR-NNNN（C1）/ body 带 Card: 元数据行 / 一个 PR 一件事 diff<400 行 =="
