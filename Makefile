# Every target here also runs in CI. If it passes locally it passes there.

GO      ?= go
MODULES  = . ./ratelimittest ./backends/redis ./metrics/prometheus
REDIS_PORT ?= 6399

.PHONY: all
all: fmt vet test race deps bench-smoke

.PHONY: fmt
fmt:
	@out=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet:
	@for m in $(MODULES); do \
	  echo "vet $$m"; (cd $$m && $(GO) vet ./...) || exit 1; \
	done

# The whole suite. Nothing in it sleeps: everything time dependent runs under
# testing/synctest or a manually advanced clock.
.PHONY: test
test:
	@for m in $(MODULES); do \
	  echo "test $$m"; (cd $$m && $(GO) test -count=1 ./...) || exit 1; \
	done

.PHONY: race
race:
	@for m in $(MODULES); do \
	  echo "race $$m"; (cd $$m && $(GO) test -count=1 -race ./...) || exit 1; \
	done

# The allocation budgets are assertions, so they fail the build. Run them alone
# so a failure is unmistakable.
.PHONY: alloc
alloc:
	$(GO) test -count=1 -run 'Alloc|Allocates' -v .

# Zero dependencies in the root module, checked rather than promised.
.PHONY: deps
deps:
	$(GO) test -count=1 -run 'TestRootModule|TestNoReflection' -v .

.PHONY: bench
bench:
	$(GO) test -run xxx -bench . -benchmem -count=6 . | tee bench.txt

.PHONY: bench-smoke
bench-smoke:
	$(GO) test -run xxx -bench . -benchmem -benchtime 200ms .

# Integration tests against a real Redis, in their own target because they need
# one running.
.PHONY: test-redis
test-redis:
	@redis-server --port $(REDIS_PORT) --save '' --appendonly no --daemonize yes --logfile /tmp/ratelimit-redis.log
	@sleep 1
	@REDIS_ADDR=localhost:$(REDIS_PORT) $(GO) -C ./backends/redis test -count=1 -race -v ./...; \
	  status=$$?; redis-cli -p $(REDIS_PORT) shutdown nosave 2>/dev/null; exit $$status

.PHONY: cover
cover:
	$(GO) test -count=1 -coverprofile=cover.out ./...
	$(GO) tool cover -func=cover.out | tail -1

.PHONY: tidy
tidy:
	@for m in $(MODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done
