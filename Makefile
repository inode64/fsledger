SHELL := /bin/sh
.DEFAULT_GOAL := check

GO ?= go
export PATH := $(CURDIR)/bin:$(shell $(GO) env GOPATH)/bin:$(PATH)

GOLANGCI_VERSION := v2.13.2
GOVULNCHECK_VERSION := v1.7.0
NILAWAY_VERSION := v0.0.0-20260802165852-32ec3a0e8a41
YAMLFMT_VERSION := v0.21.0

# Propagate package-loading failures; only a genuinely empty module is skipped.
define with_packages
	@set -eu; packages=$$($(GO) list ./...); \
	if [ -z "$$packages" ]; then \
		printf '%s\n' 'SKIP $@: There are no packages in Go yet.'; \
	else \
		$(1); \
	fi
endef

.PHONY: check tools lint-config lint nilaway vuln modules build test test-stress fmt yaml markdown fuzz

check: lint-config modules lint nilaway vuln build test

tools:
	GOBIN=$(CURDIR)/bin $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION)

lint-config:
	golangci-lint config verify

modules:
	$(GO) mod tidy -diff
	$(GO) mod verify

lint:
	$(call with_packages,golangci-lint run ./...)

nilaway:
	$(call with_packages,nilaway -include-pkgs=fsledger -exclude-test-files=false ./...)

vuln:
	$(call with_packages,govulncheck -test ./...)

build:
	$(call with_packages,$(GO) build ./...)

test:
	$(call with_packages,mkdir -p reports && $(GO) test -race -shuffle=on -count=1 -timeout=5m -covermode=atomic -coverpkg=./... -coverprofile=reports/coverage.out ./... && $(GO) tool cover -func=reports/coverage.out)

test-stress:
	$(call with_packages,$(GO) test -race -shuffle=on -count=20 -timeout=15m ./...)

fmt:
	$(call with_packages,golangci-lint fmt ./...)

yaml:
	yamlfmt -lint
	yamllint --strict . .yamlfmt

markdown:
	npx --yes markdownlint-cli@0.49.1 --config .markdownlint.yml '**/*.md' --ignore bin --ignore vendor --ignore graphify-out

# Example: make fuzz FUZZ_PACKAGE=./internal/parser FUZZ_TARGET=FuzzParse
FUZZ_TIME ?= 30s
fuzz:
	@test -n "$(FUZZ_PACKAGE)" -a -n "$(FUZZ_TARGET)" || { echo 'Indica FUZZ_PACKAGE y FUZZ_TARGET.'; exit 2; }
	$(GO) test -race -run='^$$' -fuzz='^$(FUZZ_TARGET)$$' -fuzztime=$(FUZZ_TIME) $(FUZZ_PACKAGE)
