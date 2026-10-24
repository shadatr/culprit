GO ?= go

.PHONY: build test lint fmt eval scenarios web demo-up demo-down

build:
	$(GO) build -o bin/culprit ./cmd/culprit

test:
	$(GO) test -race ./...

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

fmt:
	gofmt -w .

scenarios:
	$(GO) run ./eval/gen -out eval/scenarios

eval: build
	./bin/culprit eval -scenarios eval/scenarios -md EVAL_RESULTS.md

web:
	cd web && npm install && npm run dev

demo-up:
	$(MAKE) -C demo up

demo-down:
	$(MAKE) -C demo down
