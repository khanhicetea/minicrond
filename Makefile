VERSION ?= 0.1.0-dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

LOCAL_TEST_HOST ?= 100.117.173.93

.PHONY: build build-web restart-example-local local-test test check contracts release-check
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/minicron ./cmd/minicron

build-web:
	cd web && npm run build

restart-example-local:
	cd examples/local-test && ./stop.sh && ./start.sh

local:
	$(MAKE) build
	$(MAKE) build-web
	$(MAKE) restart-example-local
	@echo "web ui: http://$(LOCAL_TEST_HOST):7423"

test:
	go test ./...

check: test
	go vet ./...
	git diff --exit-code -- schema/minicron.schema.json cmd/minicron/openapi.json

contracts:
	cmp schema/minicron.schema.json cmd/minicron/minicron.schema.json
	go run ./cmd/openapi-gen | cmp - cmd/minicron/openapi.json
	python3 -m json.tool cmd/minicron/openapi.json >/dev/null

release-check: check contracts build
	sha256sum bin/minicron > bin/sha256sums.txt
