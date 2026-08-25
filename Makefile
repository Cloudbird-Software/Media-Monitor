# Media-Monitor local build/check equivalents of the CI gates.
# CI semantics stay authoritative in .github/workflows/ci.yml; this Makefile
# is the honest thin wrapper (CI-Workflows convention), not a CI masquerade.

SHELL := /bin/bash
GO ?= go

.PHONY: setup check card-test gates-pr adapt-offline

setup:
	@$(GO) version
	@if grep -qE '^require ' go.mod; then \
		echo "dependency policy broken: go.mod contains a require block (stdlib-only policy, docs/OPERATIONS.md)" >&2; exit 1; \
	fi
	@echo "OK toolchain + stdlib-only dependency policy"

check:
	@fmt="$$(gofmt -l .)"; if [ -n "$$fmt" ]; then echo "gofmt diff:"; echo "$$fmt" >&2; exit 1; fi
	@$(GO) vet ./...
	@bash quality/arch-check.sh
	@$(GO) test -race ./...

card-test: ## 读卡 AC 列表（org entry protocol）：make card-test CARD=<issue#>
	@test -n "$(CARD)" || { echo "用法: make card-test CARD=<issue#>（缺 CARD）" >&2; exit 2; }
	@gh issue view "$(CARD)" -R Cloudbird-Software/Media-Monitor --json number,title,body \
	  --jq '"#\(.number) \(.title)\n\n\(.body)"' 2>/dev/null \
	  | awk 'NR==1{print;print ""} /^## AC/{f=1} f{print} f && /^## / && !/^## AC/{exit}' | head -60

gates-pr: ## 本地等价质量关卡：make gates-pr
	@bash quality/run-gates.sh pr

adapt-offline: ## 跑适配层离线金丝雀：make adapt-offline
	@$(GO) run ./cmd/mediactl adapt canary --offline