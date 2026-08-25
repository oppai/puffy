# puffy - development tasks.
#
# `make` on its own lists the targets. `make check` is what CI should run.

MODULE  := github.com/oppai/puffy
BIN     := puffy
OUT     := out

GO      ?= go
GOOS    ?= $(shell $(GO) env GOOS)
GOARCH  ?= $(shell $(GO) env GOARCH)

# Stamp the real version only when git can name this commit. An untagged
# checkout keeps the default compiled into internal/probe, so `go install`
# without make reports the same thing a source build should.
GIT_VERSION := $(shell git describe --tags --dirty 2>/dev/null)
ifneq ($(GIT_VERSION),)
LDFLAGS := -X $(MODULE)/internal/probe.Version=$(GIT_VERSION)
endif
# Release binaries drop the symbol table and DWARF; local builds keep them so
# a panic in the field is still readable.
RELEASE_LDFLAGS := $(LDFLAGS) -s -w

# Cross-compile matrix. Windows is deliberately absent: it has no unprivileged
# ICMP datagram socket, so puffy would need administrator rights there and the
# traceroute path is untested.
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

# Defaults for the demo and gif targets; override on the command line.
TARGET  ?= 1.1.1.1
COUNT   ?= 60
INTERVAL ?= 400ms
# The GIF wants a shorter run: one frame per redraw, so 60 rounds would be a
# minute of animation nobody watches to the end.
GIF_ROUNDS ?= 26

.DEFAULT_GOAL := help
.PHONY: help build install test race cover cover-html bench vet fmt fmt-check \
        tidy check eyeball demo gif dist setcap clean

help: ## List the available targets
	@echo "puffy - make targets"
	@echo
	@grep -hE '^[a-z][a-zA-Z0-9_-]*:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "} {printf "  \033[1m%-12s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "variables: TARGET=$(TARGET) COUNT=$(COUNT) INTERVAL=$(INTERVAL) GIF_ROUNDS=$(GIF_ROUNDS)"

build: ## Build ./puffy for this machine
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) .

install: ## Install puffy into GOBIN
	$(GO) install -ldflags "$(LDFLAGS)" .

test: ## Run the tests
	$(GO) test ./...

race: ## Run the tests under the race detector
	$(GO) test -race -count=1 ./...

cover: ## Report per-package coverage
	$(GO) test -cover ./...

cover-html: ## Open a coverage report in a browser
	@mkdir -p $(OUT)
	$(GO) test -coverprofile=$(OUT)/coverage.out ./...
	$(GO) tool cover -html=$(OUT)/coverage.out -o $(OUT)/coverage.html
	@echo "wrote $(OUT)/coverage.html"

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Rewrite sources with gofmt
	gofmt -w -l .

fmt-check: ## Fail if anything is not gofmt'd
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then \
		echo "not gofmt'd:"; echo "$$files"; exit 1; \
	fi

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

check: fmt-check vet test race ## Everything CI should run

eyeball: ## Print the rendered terminal frames for visual inspection
	PUFFY_EYEBALL=1 $(GO) test ./internal/tui -run Eyeball -v

demo: build ## Trace TARGET and open the HTML report
	@mkdir -p $(OUT)
	./$(BIN) trace $(TARGET) --count $(COUNT) --interval $(INTERVAL) \
		--json $(OUT)/demo.json --html $(OUT)/demo.html
	@if command -v open >/dev/null 2>&1; then open $(OUT)/demo.html; \
	elif command -v xdg-open >/dev/null 2>&1; then xdg-open $(OUT)/demo.html; \
	else echo "open $(OUT)/demo.html in a browser"; fi

gif: build ## Re-record docs/demo.gif from a live run (needs Chrome + ImageMagick)
	@command -v magick >/dev/null 2>&1 || { echo "needs ImageMagick 7 (magick)"; exit 1; }
	@test -x "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
		|| { echo "needs Google Chrome as the headless renderer"; exit 1; }
	python3 docs/record-demo.py $(TARGET) $(GIF_ROUNDS)

dist: ## Cross-compile release binaries into out/
	@mkdir -p $(OUT)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			$(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" \
			-o $(OUT)/$(BIN)-$$os-$$arch . || exit 1; \
	done
	@echo; ls -lh $(OUT)/$(BIN)-* | awk '{print "  " $$5, $$9}'

setcap: build ## Linux only: allow ICMP without root for this binary
	@# One shell for the whole guard: each recipe line runs in its own shell, so
	@# an `exit 0` on an earlier line would not stop the sudo below.
	@if [ "$$($(GO) env GOOS)" != "linux" ]; then \
		echo "setcap is Linux-only. On $$($(GO) env GOOS), puffy needs no setup."; \
	else \
		sudo setcap cap_net_raw+ep ./$(BIN) && \
		echo "granted CAP_NET_RAW to ./$(BIN)"; \
	fi

clean: ## Remove build output
	rm -f $(BIN)
	rm -rf $(OUT)
	$(GO) clean -testcache
