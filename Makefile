SHELL := /bin/sh
.DEFAULT_GOAL := check

GO ?= go
PYTHON ?= python3
BUILD_DIR ?= bin
# Use the portable x86-64 baseline even when the local Go toolchain defaults
# to newer CPU instructions. An explicit GOAMD64 environment/make value wins.
GOAMD64 ?= v1
export GOAMD64
PREFIX ?= /usr
DESTDIR ?=
BINDIR ?= $(PREFIX)/bin
SBINDIR ?= $(PREFIX)/sbin
MANDIR ?= $(PREFIX)/share/man
SYSCONFDIR ?= /etc
INITDIR ?= $(SYSCONFDIR)/init.d
CONFDIR ?= $(SYSCONFDIR)/conf.d
SYSTEMDUNITDIR ?= /etc/systemd/system
INSTALL ?= install
# Embedded release identifier; override with make build VERSION=v1.2.3. Without
# Git it stays empty and the binaries fall back to Go module/VCS build metadata.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS = -X github.com/inode64/fsledger/internal/cli.version=$(VERSION)
TOOLS_VENV := $(CURDIR)/bin/tools-venv
export PATH := $(CURDIR)/bin:$(TOOLS_VENV)/bin:$(CURDIR)/bin/node/node_modules/.bin:$(shell $(GO) env GOPATH)/bin:$(PATH)

include tools/versions.mk

# Propagate package-loading failures; only a genuinely empty module is skipped.
define with_packages
	@set -eu; packages=$$($(GO) list ./...); \
	if [ -z "$$packages" ]; then \
		printf '%s\n' 'SKIP $@: There are no packages in Go yet.'; \
	else \
		$(1); \
	fi
endef

.PHONY: install install-bin install-man install-config install-openrc install-systemd
.PHONY: check tools lint-config lint nilaway vuln modules build test test-stress fmt yaml markdown fuzz
.PHONY: check-static check-security tools-go tools-python tools-node
.PHONY: fmt-check deadcode lint-shell lint-workflows secrets secrets-staged secrets-history vuln-binaries publication-check
.PHONY: release

check: check-static check-security build test

check-static: lint-config modules fmt-check lint nilaway deadcode lint-shell lint-workflows yaml markdown

check-security: publication-check vuln vuln-binaries secrets secrets-staged secrets-history

tools: tools-go tools-python tools-node

tools-go:
	GOBIN=$(CURDIR)/bin $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install golang.org/x/tools/cmd/deadcode@$(GOTOOLS_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	GOBIN=$(CURDIR)/bin $(GO) install github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION)

tools-python:
	$(PYTHON) -m venv "$(TOOLS_VENV)"
	"$(TOOLS_VENV)/bin/python" -m pip install --disable-pip-version-check \
		yamllint==$(YAMLLINT_VERSION) shellcheck-py==$(SHELLCHECK_PY_VERSION)

tools-node:
	npm install --prefix "$(CURDIR)/bin/node" --no-audit --no-fund --save-exact markdownlint-cli@$(MARKDOWNLINT_VERSION)

lint-config:
	golangci-lint config verify

modules:
	$(GO) mod tidy -diff
	$(GO) mod verify

lint:
	$(call with_packages,golangci-lint run ./...)

nilaway:
	$(call with_packages,nilaway -include-pkgs=github.com/inode64/fsledger -exclude-test-files=false ./...)

vuln:
	$(call with_packages,govulncheck -test ./...)

vuln-binaries: build
	govulncheck -mode=binary "$(BUILD_DIR)/fsledger"
	govulncheck -mode=binary "$(BUILD_DIR)/fsledgerd"

deadcode:
	@set -eu; output=$$(mktemp); trap 'rm -f "$$output"' 0; \
		deadcode ./cmd/fsledger ./cmd/fsledgerd > "$$output"; \
		cat "$$output"; test ! -s "$$output"

lint-shell:
	shellcheck --shell=sh packaging/openrc/fsledger.initd packaging/openrc/fsledger.confd

lint-workflows:
	actionlint

secrets:
	gitleaks dir --redact --no-banner --config .gitleaks.toml .

secrets-staged:
	gitleaks git --pre-commit --staged --redact --no-banner --config .gitleaks.toml .

secrets-history:
	gitleaks git --redact --no-banner --config .gitleaks.toml --log-opts=--all .

# Ignoring a path does not remove an already staged file from a future commit.
publication-check:
	@set -eu; paths=$$(git ls-files -- \
		':(top)AGENTS.md' ':(top)hosts' ':(top)docs' ':(top)scripts' \
		':(top)reports' ':(top)graphify-out' ':(top)bin' \
		':(top).idea' ':(top).vscode' ':(top).history' ':(top).codex' ':(top).claude' \
		':(top).env' ':(top).env.*' ':(top,exclude).env.example'); \
		if [ -n "$$paths" ]; then \
			printf '%s\n' 'Private files or generated artifacts are staged or tracked:' "$$paths" >&2; \
			exit 1; \
		fi

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o "$(BUILD_DIR)/fsledger" ./cmd/fsledger
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o "$(BUILD_DIR)/fsledgerd" ./cmd/fsledgerd

# Release assets always target the portable Linux x86-64 baseline.
release:
	@case "$(VERSION)" in \
		''|[!a-zA-Z0-9]*|*[!a-zA-Z0-9._+-]*) \
			echo 'VERSION must start with a letter or digit and contain only letters, digits, dots, underscores, + or -.' >&2; \
			exit 2 ;; \
		esac
	$(MAKE) build BUILD_DIR=bin/release GOOS=linux GOARCH=amd64 GOAMD64=v1 VERSION="$(VERSION)"
	mv bin/release/fsledger bin/release/fsledger-linux-x86_64
	mv bin/release/fsledgerd bin/release/fsledgerd-linux-x86_64
	$(INSTALL) -m 0644 LICENSE bin/release/LICENSE
	cd bin/release && sha256sum fsledger-linux-x86_64 fsledgerd-linux-x86_64 LICENSE > SHA256SUMS

test:
	$(call with_packages,mkdir -p bin && $(GO) test -race -shuffle=on -count=1 -timeout=5m -covermode=atomic -coverpkg=./... -coverprofile=bin/coverage.out ./... && $(GO) tool cover -func=bin/coverage.out)

test-stress:
	$(call with_packages,$(GO) test -race -shuffle=on -count=20 -timeout=15m ./...)

fmt:
	$(call with_packages,golangci-lint fmt ./...)

fmt-check:
	@set -eu; output=$$(mktemp); trap 'rm -f "$$output"' 0; \
		golangci-lint fmt --diff ./... > "$$output"; \
		cat "$$output"; test ! -s "$$output"

yaml:
	yamlfmt -lint
	yamllint --strict . .yamlfmt

markdown:
	markdownlint --config .markdownlint.yml '**/*.md' --ignore bin --ignore vendor --ignore graphify-out

# Example: make fuzz FUZZ_PACKAGE=./internal/parser FUZZ_TARGET=FuzzParse
FUZZ_TIME ?= 30s
fuzz:
	@test -n "$(FUZZ_PACKAGE)" -a -n "$(FUZZ_TARGET)" || { echo 'Indica FUZZ_PACKAGE y FUZZ_TARGET.'; exit 2; }
	$(GO) test -race -run='^$$' -fuzz='^$(FUZZ_TARGET)$$' -fuzztime=$(FUZZ_TIME) $(FUZZ_PACKAGE)

# DESTDIR stages packaging without embedding the staging directory in service paths.
install: install-bin install-man install-config

install-bin: build
	$(INSTALL) -d "$(DESTDIR)$(BINDIR)" "$(DESTDIR)$(SBINDIR)"
	$(INSTALL) -m 0755 "$(BUILD_DIR)/fsledger" "$(DESTDIR)$(BINDIR)/fsledger"
	$(INSTALL) -m 0755 "$(BUILD_DIR)/fsledgerd" "$(DESTDIR)$(SBINDIR)/fsledgerd"

install-man:
	$(INSTALL) -d "$(DESTDIR)$(MANDIR)/man1" "$(DESTDIR)$(MANDIR)/man5" "$(DESTDIR)$(MANDIR)/man8"
	$(INSTALL) -m 0644 packaging/man/fsledger.1 "$(DESTDIR)$(MANDIR)/man1/fsledger.1"
	$(INSTALL) -m 0644 packaging/man/fsledger.yaml.5 "$(DESTDIR)$(MANDIR)/man5/fsledger.yaml.5"
	$(INSTALL) -m 0644 packaging/man/fsledgerd.8 "$(DESTDIR)$(MANDIR)/man8/fsledgerd.8"

# Existing configuration, including symlinks, is never overwritten.
install-config:
	$(INSTALL) -d -m 0700 "$(DESTDIR)$(SYSCONFDIR)/fsledger"
	@if [ ! -e "$(DESTDIR)$(SYSCONFDIR)/fsledger/fsledger.yaml" ] && [ ! -L "$(DESTDIR)$(SYSCONFDIR)/fsledger/fsledger.yaml" ]; then \
		$(INSTALL) -m 0600 examples/fsledger.yaml "$(DESTDIR)$(SYSCONFDIR)/fsledger/fsledger.yaml"; \
	fi

	@for category in services notifiers templates excludes; do \
		$(INSTALL) -d -m 0700 "$(DESTDIR)$(SYSCONFDIR)/fsledger/$$category"; \
		for source in examples/$$category/*; do \
			target="$(DESTDIR)$(SYSCONFDIR)/fsledger/$$category/$${source##*/}"; \
			if [ ! -e "$$target" ] && [ ! -L "$$target" ]; then $(INSTALL) -m 0600 "$$source" "$$target"; fi; \
		done; \
	done

# Service installation does not enable or start a service.
install-openrc: install
	$(INSTALL) -d "$(DESTDIR)$(INITDIR)" "$(DESTDIR)$(CONFDIR)"
	sed -e 's|@SBINDIR@|$(SBINDIR)|g' -e 's|@BINDIR@|$(BINDIR)|g' -e 's|@SYSCONFDIR@|$(SYSCONFDIR)|g' packaging/openrc/fsledger.initd > "$(DESTDIR)$(INITDIR)/fsledger"
	chmod 0755 "$(DESTDIR)$(INITDIR)/fsledger"
	@if [ ! -e "$(DESTDIR)$(CONFDIR)/fsledger" ] && [ ! -L "$(DESTDIR)$(CONFDIR)/fsledger" ]; then \
		sed 's|@SYSCONFDIR@|$(SYSCONFDIR)|g' packaging/openrc/fsledger.confd > "$(DESTDIR)$(CONFDIR)/fsledger"; \
		chmod 0644 "$(DESTDIR)$(CONFDIR)/fsledger"; \
	fi

install-systemd: install
	$(INSTALL) -d "$(DESTDIR)$(SYSTEMDUNITDIR)"
	sed -e 's|/usr/sbin|$(SBINDIR)|g' -e 's|/etc/fsledger|$(SYSCONFDIR)/fsledger|g' packaging/fsledger.service > "$(DESTDIR)$(SYSTEMDUNITDIR)/fsledger.service"
	chmod 0644 "$(DESTDIR)$(SYSTEMDUNITDIR)/fsledger.service"

.PHONY: generate-catalog
generate-catalog:
	GOBIN=$(CURDIR)/bin $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	protoc --go_out=. --go_opt=paths=source_relative internal/catalog/recordpb/record.proto
