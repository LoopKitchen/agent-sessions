# loop-sessions build and release.
#
# The release target produces exactly what install/install.sh and the agent's
# self-upgrade expect to find on the release host: one binary per platform,
# named loop-sessions_<os>_<arch>, a SHA256SUMS manifest covering all of them,
# and latest.json, which names the build (version, commit, date, capture
# schema) and the digest of every asset. Those names are a contract between
# this file, that script, internal/upgrade and server/app/download.go;
# changing one without the others breaks installs silently, since the
# installer would simply report that no build exists for the platform.

BIN        := loop-sessions
PKG        := ./cmd/loop-sessions
DIST       := dist

# Version comes from git when available. A tagged build gets the tag; anything
# else gets a describe string that names the commit, so a binary in the wild can
# always be traced back to a tree. COMMIT is the full sha for latest.json, which
# is what the fleet evaluator compares health reports against; a 7-character
# prefix is enough for a person and not enough for a machine.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

# BUILD_DATE is evaluated exactly once and handed to the sub-make that writes
# the manifest. With `?=` it was a recursive variable: LDFLAGS froze one
# reading of the clock, and the manifest recipe, running in its own make,
# took another a few cross-compiles later, so the binary's BuildDate and
# latest.json's build_date disagreed by seconds on every archive build (the
# one kind of build the flag exists for). An explicit BUILD_DATE on the
# command line or in the environment is still honoured.
ifndef BUILD_DATE
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
endif
export BUILD_DATE

# The capture schema the binary emits, read from the constant rather than
# typed here so latest.json cannot claim a version the code does not.
CAPTURE_SCHEMA := $(shell sed -n 's/^const CaptureSchema = \([0-9][0-9]*\)$$/\1/p' internal/event/event.go)

# Reproducibility, and the reasons:
#   CGO_ENABLED=0  a static binary that runs on a machine without a toolchain,
#                  and removes the host C library from the output.
#   -trimpath      strips local filesystem paths, so two people building the same
#                  commit get byte-identical output and no home directory leaks
#                  into the shipped binary.
#   -s -w          drops the symbol table and DWARF; smaller download.
GOFLAGS    := -trimpath

# Stamped into the binary so an installed agent works with no flags. It is not
# a secret: it is a URL people paste into a browser anyway. There is no default:
# set ENDPOINT to the base URL of the server you run, and a binary built
# without one asks for --endpoint or LOOP_SESSIONS_ENDPOINT at install time
# (cmd/loop-sessions/setup.go, resolveEndpoint) rather than silently enrolling
# against a placeholder host:
#
#   make release ENDPOINT=https://sessions.example.com
#
# No OAuth client id is stamped alongside it. Enrollment signs in through the
# server's own page against the server's Firebase project, so the agent holds
# no client credential at all.
#
# BuildDate is stamped for the builds the toolchain cannot stamp itself (a git
# archive has no .git and gets no vcs.time); the health report carries
# whichever the binary has.
ENDPOINT   ?=
LDFLAGS    := -s -w -X main.Version=$(VERSION) \
              -X main.BuildDate=$(BUILD_DATE) \
              -X main.defaultEndpoint=$(ENDPOINT)
BUILDENV   := CGO_ENABLED=0

PLATFORMS  := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

# macOS ships shasum; most Linux images ship sha256sum.
SHA256 := $(shell command -v sha256sum 2>/dev/null || echo "shasum -a 256")

# Shell scripts that must parse and pass shellcheck.
SCRIPTS := install/install.sh install/uninstall.sh examples/deploy-gcp/ci-setup.sh

.DEFAULT_GOAL := help
.PHONY: help build test lint fmt release verify clean

help: ## show this help
	@printf 'loop-sessions %s\n\n' '$(VERSION)'
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-10s\033[0m %s\n", $$1, $$2}'
	@printf '\n'

build: ## build for this machine into ./loop-sessions
	$(BUILDENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)
	@printf 'built %s %s\n' '$(BIN)' '$(VERSION)'

test: ## run the full suite with the race detector
	go test -race ./...

# gofmt -l prints the names of unformatted files and exits zero, so the exit
# status has to be derived from whether it printed anything. Without this the
# target passes on a badly formatted tree.
lint: ## fail if anything is unformatted or vet-unclean
	@out="$$(gofmt -l . 2>/dev/null)"; \
	if [ -n "$$out" ]; then \
		printf 'gofmt: these files need formatting:\n%s\n' "$$out" >&2; \
		exit 1; \
	fi
	@printf 'gofmt: clean\n'
	go vet ./...
	@printf 'go vet: clean\n'
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck -s sh $(SCRIPTS) && printf 'shellcheck: clean\n'; \
	else \
		printf 'shellcheck: not installed, skipping\n'; \
	fi
	@for s in $(SCRIPTS); do sh -n "$$s" || exit 1; done && printf 'sh -n: clean\n'

fmt: ## format everything
	gofmt -w .

# A release is cut from a clean checkout, or not at all. A build that a fleet
# once ran for weeks carried vcs.modified=true and a vcs.revision that names
# no commit in this repository, so nobody could reproduce its bytes; refusing
# a dirty tree is how that stops recurring. The tree check alone is not
# enough: the toolchain stamps a linked worktree with its primary checkout's
# HEAD (it looks for a .git directory, and a worktree has a .git file), so a
# clean worktree yields a binary whose agent_commit the fleet evaluator can
# never match to latest.json. The stamp is therefore compared with COMMIT
# after the build, before any manifest is written. ALLOW_DIRTY=1 skips both
# checks for a scratch build against a scratch endpoint, never for anything
# that reaches a bucket.
release: clean ## cross-compile all platforms, write dist/SHA256SUMS and dist/latest.json
	@if [ -z "$(ALLOW_DIRTY)" ] && [ -n "$$(git status --porcelain 2>/dev/null)" ]; then \
		printf 'release: the working tree is dirty; commit or stash first (ALLOW_DIRTY=1 overrides for scratch builds):\n' >&2; \
		git status --porcelain >&2; \
		exit 1; \
	fi
	@test -n "$(CAPTURE_SCHEMA)" || { printf 'release: cannot read CaptureSchema from internal/event/event.go\n' >&2; exit 1; }
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os="$${p%/*}"; arch="$${p#*/}"; \
		out="$(DIST)/$(BIN)_$${os}_$${arch}"; \
		printf 'building %s\n' "$$out"; \
		$(BUILDENV) GOOS=$$os GOARCH=$$arch \
			go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o "$$out" $(PKG) || exit 1; \
	done
	@if [ -z "$(ALLOW_DIRTY)" ]; then \
		stamp="$$(go version -m $(DIST)/$(BIN)_linux_amd64)"; \
		rev="$$(printf '%s\n' "$$stamp" | sed -n 's/.*vcs\.revision=\([0-9a-f]*\).*/\1/p')"; \
		if [ "$$rev" != "$(COMMIT)" ]; then \
			printf 'release: the binary is stamped vcs.revision=%s but latest.json would say %s; build from the checkout that owns .git (a linked worktree is stamped with its primary checkout) or pass ALLOW_DIRTY=1 for a scratch build\n' "$${rev:-none}" '$(COMMIT)' >&2; \
			exit 1; \
		fi; \
		if printf '%s\n' "$$stamp" | grep -q 'vcs.modified=true'; then \
			printf 'release: the binary carries vcs.modified=true; refusing to write a manifest for it\n' >&2; \
			exit 1; \
		fi; \
	fi
	@cd $(DIST) && $(SHA256) $(BIN)_* > SHA256SUMS
	@printf '\n%s\n' 'SHA256SUMS:'
	@cat $(DIST)/SHA256SUMS
	@$(MAKE) --no-print-directory manifest
	@printf '\n%s\n' 'go version -m (the stamp the fleet will report):'
	@go version -m $(DIST)/$(BIN)_linux_amd64 | grep -E '^\s+(build\s+(vcs|-ldflags|CGO_ENABLED|-trimpath)|path|mod)' || true
	@printf '\nUpload the contents of %s/ to the release host.\n' '$(DIST)'
	@printf 'install.sh fetches <base>/<channel>/SHA256SUMS, <base>/<channel>/latest.json and <base>/<channel>/$(BIN)_<os>_<arch>.\n'

# latest.json names the build and its assets. Written from SHA256SUMS rather
# than from the build loop so the two cannot disagree about a digest.
manifest:
	@test -f $(DIST)/SHA256SUMS || { printf 'no %s/SHA256SUMS; run make release first\n' '$(DIST)' >&2; exit 1; }
	@{ \
		printf '{\n  "version": "%s",\n  "commit": "%s",\n  "build_date": "%s",\n  "capture_schema": %s,\n  "assets": {' \
			'$(VERSION)' '$(COMMIT)' '$(BUILD_DATE)' '$(CAPTURE_SCHEMA)'; \
		awk '{ name = $$2; sub(/^\*/, "", name); printf "%s\n    \"%s\": \"%s\"", (NR > 1 ? "," : ""), name, $$1 } END { printf "\n  }\n}\n" }' $(DIST)/SHA256SUMS; \
	} > $(DIST)/latest.json
	@printf '\n%s\n' 'latest.json:'
	@cat $(DIST)/latest.json

verify: ## re-check dist/ against its own manifest
	@test -f $(DIST)/SHA256SUMS || { printf 'no %s/SHA256SUMS; run make release first\n' '$(DIST)' >&2; exit 1; }
	@cd $(DIST) && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum -c SHA256SUMS; \
	else \
		shasum -a 256 -c SHA256SUMS; \
	fi
	@test -f $(DIST)/latest.json || { printf 'no %s/latest.json; run make release first\n' '$(DIST)' >&2; exit 1; }
	@if command -v python3 >/dev/null 2>&1; then \
		python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["version"] and d["commit"] and d["assets"], d' $(DIST)/latest.json && printf 'latest.json: valid\n'; \
	fi

clean: ## remove build output
	rm -rf $(DIST) $(BIN)
