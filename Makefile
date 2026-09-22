# Structura developer workflow.
#
# CGO_ENABLED=0 is set for every Go invocation, not just the release build.
# The distribution promise (static binaries, five platforms, no external
# dependencies) holds only while nothing in the tree pulls in CGO, and the
# cheapest place to catch an accidental CGO dependency is the moment it is
# added.
export CGO_ENABLED := 0

BIN      := bin/structura
PKG      := github.com/kamronarabi/structura
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CANONICAL := grep -v -E '"(generatedAt|durationMs)"'
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.DEFAULT_GOAL := check

## build: compile the CLI to bin/structura
.PHONY: build
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/structura

## test: fast unit tests (the pre-commit loop)
.PHONY: test
test:
	go test ./... -short
# The eval harness's grader decides whether a release-gating run passes, so it
# is unit-tested like anything else. These tests make no API calls.
	go test -tags eval ./internal/devtools/eval/...

## test-race: full suite under the race detector
.PHONY: test-race
test-race:
	go test -race ./...

## golden: regenerate golden fixtures, then show what moved
.PHONY: golden
golden:
	STRUCTURA_GOLDEN=update go test ./...
	@git --no-pager diff --stat -- '*testdata*' || true

## lint: vet + golangci-lint
.PHONY: lint
lint:
	go vet ./...
	golangci-lint run ./...
# The corpus and eval packages sit behind build tags, so the sweep above never
# compiles them. Left unchecked they rot quietly and only break on the day
# someone needs them.
	go vet -tags corpus ./internal/resolve/...
	go vet -tags eval ./internal/devtools/eval/...
	golangci-lint run --build-tags corpus ./internal/resolve/...
	golangci-lint run --build-tags eval ./internal/devtools/eval/...

## fmt: apply formatting and import grouping
.PHONY: fmt
fmt:
	golangci-lint fmt ./...

## tidy: sync go.mod/go.sum and fail if that produced a diff
.PHONY: tidy
tidy:
	go mod tidy
	@git diff --exit-code -- go.mod go.sum

## cgo-free: fail if anything pulled CGO into the dependency graph
.PHONY: cgo-free
cgo-free:
	@if go list -deps ./... | grep -qx 'runtime/cgo'; then \
	  echo "runtime/cgo is in the dependency graph; static cross-compilation is broken"; \
	  exit 1; \
	fi; \
	echo "cgo-free: OK"

## determinism: two scans of an unchanged tree must be byte-identical
.PHONY: determinism
determinism: build
	@set -e; \
	found=0; \
	for repo in $${REPO:-testdata/golden/*}; do \
	  [ -d "$$repo" ] || continue; \
	  found=1; \
	  tmp=$$(mktemp -d); \
	  cp -R "$$repo" "$$tmp/repo"; \
	  ./$(BIN) scan --root "$$tmp/repo" --quiet; \
	  $(CANONICAL) "$$tmp/repo/.structura/graph.json" > "$$tmp/first.json"; \
	  ./$(BIN) scan --root "$$tmp/repo" --quiet; \
	  $(CANONICAL) "$$tmp/repo/.structura/graph.json" > "$$tmp/second.json"; \
	  if ! cmp -s "$$tmp/first.json" "$$tmp/second.json"; then \
	    echo "NON-DETERMINISTIC: two scans of $$repo differ"; \
	    diff -u "$$tmp/first.json" "$$tmp/second.json" | head -40; \
	    rm -rf "$$tmp"; exit 1; \
	  fi; \
	  rm -rf "$$tmp"; \
	  echo "  $$repo: identical"; \
	done; \
	if [ "$$found" = 0 ]; then echo "determinism: no fixtures yet, skipping"; else echo "determinism: OK"; fi

## fuzz: short fuzz run over every target (CI gate)
.PHONY: fuzz
fuzz:
	@for pkg in $$(go list ./... ); do \
	  for target in $$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
	    echo "fuzzing $$target in $$pkg"; \
	    go test $$pkg -run '^$$' -fuzz "^$$target$$" -fuzztime 60s || exit 1; \
	  done; \
	done

## demo: scan a fixture repo and pretty-print the result
.PHONY: demo
demo: build
	./$(BIN) scan --root $${REPO:-testdata/golden/compose-monolith} --print

## corpus: clone the pinned real-world repos used by the slow resolver tests
.PHONY: corpus
corpus:
	go run ./internal/devtools/corpus -config testdata/corpus.yaml

## corpus-test: score resolver precision/recall against the cloned corpus
.PHONY: corpus-test
corpus-test:
	go test -tags corpus ./internal/resolve/... -v

## eval: ask a model 18 architecture questions through the MCP tools
# Not part of `check`: it calls the API, it costs money, and it is
# nondeterministic. A flaky gate gets disabled, and then it stops being run at
# all. Run it per milestone and before each release.
.PHONY: eval
eval:
	go run -tags eval ./internal/devtools/eval $(EVAL_ARGS)

## snapshot: exercise the full release pipeline without publishing
.PHONY: snapshot
snapshot:
	goreleaser build --snapshot --clean --single-target

## check: what CI runs on every PR
.PHONY: check
check: tidy lint cgo-free test-race determinism

## clean: remove build and scan artifacts
.PHONY: clean
clean:
	rm -rf bin dist .structura

## help: list targets
.PHONY: help
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F': ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
