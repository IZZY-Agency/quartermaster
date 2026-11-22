# quartermaster. `make check` runs everything CI runs; `make build` produces bin/qm.
# Each step skips itself, and says so, when its input does not exist yet.

SHELL := /bin/bash
.DEFAULT_GOAL := build

GOLANGCI_VERSION := v2.15.0
GOLANGCI := $(shell command -v golangci-lint 2>/dev/null || echo go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION))

# RELEASE_VERSION is the one source of truth for the release version: `make
# release` stamps it into the binaries, scripts/publish.sh tags it, and
# CHANGELOG.md must open with its entry (changelog-check). A dev build (`make
# build`) prints `git describe` instead.
RELEASE_VERSION := 0.2.0
RELEASE_DIR := bin/release/$(RELEASE_VERSION)
# Published builds sign (and notarize) the macOS binaries; empty: unsigned.
SIGN_IDENTITY ?=
NOTARY_PROFILE ?=
CHANGELOG ?= CHANGELOG.md

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/IZZY-Agency/quartermaster/internal/buildinfo.Version=$(VERSION) \
           -X github.com/IZZY-Agency/quartermaster/internal/buildinfo.Commit=$(COMMIT)

HAS_GO  := $(wildcard go.mod)
HAS_CMD := $(wildcard cmd/qm/main.go)

.PHONY: check build install lint test private private-all print-version privcheck \
	changelog-check notices notices-check release verify-release release-test release-test-linux sweep

check: lint test changelog-check notices-check private release-test
	@echo "make check: all steps passed"

build:
ifneq ($(HAS_CMD),)
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/qm ./cmd/qm
else
	@echo "skip: binary build (no cmd/qm/)"
endif

# privcheck builds the macOS privacy helper, bin/qm-privcheck. Without a Swift
# compiler it skips, and says so, so `make install` still installs qm and the
# worker plugin. /usr/bin/swiftc ships with macOS as a shim that works only
# once the Xcode command-line tools are installed, so that one counts only when
# xcode-select names a developer folder. (A release needs the helper:
# scripts/package.sh fails without a compiler.)
SWIFTC ?= swiftc
HAVE_SWIFTC := $(shell p="$$(command -v "$(SWIFTC)" 2>/dev/null)" && { [ "$$p" != /usr/bin/swiftc ] || xcode-select -p >/dev/null 2>&1; } && echo yes)
IS_DARWIN := $(filter Darwin,$(shell uname -s))

privcheck:
ifeq ($(IS_DARWIN),)
	@echo "skip: privacy helper (macOS only)"
else ifeq ($(HAVE_SWIFTC),)
	@echo "skip: privacy helper (no swiftc)"
else
	@mkdir -p bin
	$(SWIFTC) -O swift/privcheck.swift -o bin/qm-privcheck
endif

# install builds qm and the privacy helper first, then copies what exists into PREFIX; each step skips itself, and says so,
# when its input has not been built or does not exist yet.
PREFIX ?= $(HOME)/.local
PLUGIN_DEST := $(PREFIX)/share/quartermaster/plugins/qm-worker

install: build privcheck
	@test -n "$(PREFIX)" || { echo "install: PREFIX is empty" >&2; exit 1; }
	@if [ -f bin/qm ]; then \
		install -d "$(PREFIX)/bin" && install -m 0755 bin/qm "$(PREFIX)/bin/qm" && echo "installed: $(PREFIX)/bin/qm"; \
	else echo "skip: install qm (no bin/qm; run make build)"; fi
	@if [ -n "$(IS_DARWIN)" ] && [ -z "$(HAVE_SWIFTC)" ]; then echo "skip: privacy helper (no swiftc)"; \
	elif [ -f bin/qm-privcheck ]; then \
		install -d "$(PREFIX)/bin" && install -m 0755 bin/qm-privcheck "$(PREFIX)/bin/qm-privcheck" && echo "installed: $(PREFIX)/bin/qm-privcheck"; \
	else echo "skip: install qm-privcheck (not built)"; fi
	@if [ -d plugins/qm-worker ]; then \
		rm -rf "$(PLUGIN_DEST)" && install -d "$(PLUGIN_DEST)" && cp -R plugins/qm-worker/. "$(PLUGIN_DEST)/" && echo "installed: $(PLUGIN_DEST)"; \
	else echo "skip: install qm-worker plugin (no plugins/qm-worker/)"; fi

ifneq ($(HAS_GO),)
lint:
	$(GOLANGCI) run ./...

test:
	go test -race ./...
else
lint test:
	@echo "skip: $@ (no go.mod)"
endif

# PRIVATE_FLAGS: CI passes --no-denylist (it has no deny-list); locally the list
# is required (scripts/make-denylist.sh builds it, outside the repository).
private:
	bash scripts/check-private_test.sh
	bash scripts/check-private.sh $(PRIVATE_FLAGS)
	bash scripts/check-private.sh --history --scope=branch $(PRIVATE_FLAGS)

# private-all is the full scan: the tree, and the history of every ref (other
# worktrees' branches, the stash, notes, tags). make check scans this branch
# only. Run this before every publication, and
# scripts/check-private.sh --history --pull-refs before going public.
private-all:
	bash scripts/check-private.sh $(PRIVATE_FLAGS)
	bash scripts/check-private.sh --history $(PRIVATE_FLAGS)

# changelog-check fails unless $(CHANGELOG) opens (its first "## " heading)
# with the entry of $(RELEASE_VERSION).
changelog-check:
	@first="$$(sed -n 's/^## //p' "$(CHANGELOG)" | head -1)"; \
	if [ "$$first" != "$(RELEASE_VERSION)" ]; then \
		echo "changelog-check: $(CHANGELOG) must open with '## $(RELEASE_VERSION)' (its first entry: '$$first')" >&2; exit 1; \
	fi

# THIRD_PARTY_NOTICES.md lists the licence of everything the binaries carry
# besides quartermaster's own code: Go, and the Go modules in the build (none
# today). notices-check fails when it is out of date, so nothing new ships
# without an entry.
ifneq ($(HAS_GO),)
notices:
	bash scripts/notices.sh

notices-check:
	bash scripts/notices.sh --check
	bash scripts/notices_test.sh
else
notices notices-check:
	@echo "skip: $@ (no go.mod)"
endif

# release builds the four release archives (macOS and Linux, arm64 and
# x86_64; the macOS ones with the Swift helper) into $(RELEASE_DIR). It runs
# only in a clean clone of the public repository at tag v$(RELEASE_VERSION);
# PACKAGE_DEV=1 makes a local test build anywhere. scripts/release.sh runs it
# with the signing identity; see scripts/README.md.
# notices-check first: a release never packs a stale THIRD_PARTY_NOTICES.md.
release: notices-check
	SIGN_IDENTITY="$(SIGN_IDENTITY)" NOTARY_PROFILE="$(NOTARY_PROFILE)" \
		bash scripts/package.sh $(RELEASE_VERSION) $(RELEASE_DIR)

# verify-release checks the archives in $(RELEASE_DIR) and runs those this
# machine can run; each must print this checkout's commit.
# REQUIRE_SIGNATURE=1 also checks the macOS signatures and notarization;
# VERIFY_FLAGS=--rebuild builds them again and compares the bytes.
verify-release:
	REQUIRE_SIGNATURE="$(REQUIRE_SIGNATURE)" bash scripts/verify-release.sh $(RELEASE_DIR) $(if $(filter 1,$(PACKAGE_DEV)),dev-)$(RELEASE_VERSION) $(shell git rev-parse --short HEAD) $(VERIFY_FLAGS)

release-test:
	bash scripts/release_test.sh

# release-test-linux runs the release tests and the private-material guard's
# tests on Linux (GNU grep, sed and friends; the public repository's CI runs on
# Linux too), in a container, from a clone of what is committed: a worktree's
# .git file points at a host path the container cannot see, so the container
# gets a plain clone (--no-local: its own object files, not hard links into
# this repository's, which the container could not always read), and it clones that again (through git's own protocol,
# not by copying files across the mount) so nothing is written into the mount.
# Uncommitted changes are not tested. Ctrl-C stops the container: --init runs
# tini as its first process, and TINI_KILL_PROCESS_GROUP=1 has it pass the
# signal to every process in it. Needs docker.
LINUX_IMAGE ?= golang:1.27
release-test-linux:
	@command -v docker >/dev/null || { echo "release-test-linux: needs docker" >&2; exit 2; }
	@if [ -n "$$(git status --porcelain)" ]; then echo "release-test-linux: uncommitted changes are not tested; the clone sees commits only" >&2; fi
	@tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT && \
	git -c advice.detachedHead=false clone -q --no-local . "$$tmp/src" && \
	echo "release-test-linux: $$(git -C "$$tmp/src" log -1 --format='%h %s') in $(LINUX_IMAGE)" && \
	docker run --rm --init -e TINI_KILL_PROCESS_GROUP=1 -v "$$tmp/src":/src:ro $(LINUX_IMAGE) \
	  bash -c 'git config --global --add safe.directory "*" && git -c advice.detachedHead=false clone -q --no-local /src /w && cd /w \
	    && make release-test && bash scripts/check-private_test.sh'

# sweep: the optional semantic pre-publish sweep (internal/tools/sweep) over
# the comments and documentation of the export set; SINCE=<ref> reads only
# the files changed since that ref. Without TYPESAFE_API_KEY it says so and
# exits 0.
SINCE ?=
sweep:
	go run ./internal/tools/sweep $(if $(SINCE),-since "$(SINCE)")

print-version:
	@echo $(RELEASE_VERSION)
