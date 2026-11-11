.PHONY: fmt fmtcheck test race vet bench baseline testdoc doccheck cross stress fuzz check verify examples clean

PLATFORMS := linux/amd64 linux/386 darwin/arm64 windows/amd64 freebsd/amd64 js/wasm

EXAMPLE_DIR := build/examples
EXAMPLES := tcp-echo tcp-client udp-echo udp-client http-server socks5-server

fmt:
	gofmt -w .

fmtcheck:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

test:
	go test ./...

race:
	go test -race ./...
	go test -race -tags netxdebug ./...

vet:
	go vet ./...
	go vet -tags stress ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

# Gateway-scale baselines recorded in docs/PERFORMANCE.md: idle connection
# memory, request/response latency, a client that stops reading. Needs a
# file-descriptor limit above 2 x BASELINE_CONNS.
BASELINE_CONNS ?= 9000
baseline:
	go run ./internal/tools/baseline -idle $(BASELINE_CONNS)

testdoc:
	go run ./internal/tools/testdoc -w

doccheck:
	go run ./internal/tools/testdoc

cross:
	@for p in $(PLATFORMS); do \
		echo "vet $$p"; \
		GOOS=$${p%/*} GOARCH=$${p#*/} go vet ./... || exit 1; \
		GOOS=$${p%/*} GOARCH=$${p#*/} go vet -tags stress ./... || exit 1; \
	done

# Stress tests live behind the stress build tag. NETX_STRESS_SECONDS sets
# the long-run duration (default 3).
stress:
	go test -race -tags stress -count=1 -run Stress ./...
	go test -race -tags 'stress netxdebug' -count=1 -run Stress ./...

FUZZTIME ?= 30s
# Minimizing a new input can take up to 60s by default and shows up as
# 0 execs/sec; cap it so the fuzz time goes to fuzzing.
FUZZMIN ?= 5s
FUZZ_TARGETS := FuzzDropCount:./internal/sys FuzzDestination:./internal/sys FuzzTableOps:./flow FuzzRingOps:./queue \
	FuzzGetRelease:./buf FuzzReplyAndWriteTo:./udp FuzzReader:./stream

fuzz:
	@for t in $(FUZZ_TARGETS); do \
		echo "fuzz $${t%%:*} ($(FUZZTIME))"; \
		go test -run '^$$' -fuzz "^$${t%%:*}$$" -fuzztime $(FUZZTIME) -fuzzminimizetime $(FUZZMIN) $${t#*:} || exit 1; \
	done

check: test race vet

# verify is the required check before every push. See docs/CONTRIBUTING.md.
# test runs without -race too: allocation assertions are skipped under race.
verify: fmtcheck vet doccheck test race cross

examples:
	@mkdir -p $(EXAMPLE_DIR)
	@for example in $(EXAMPLES); do \
		echo "building $(EXAMPLE_DIR)/$$example"; \
		go build -o "$(EXAMPLE_DIR)/$$example" "./examples/$$example" || exit 1; \
	done

clean:
	rm -rf build
