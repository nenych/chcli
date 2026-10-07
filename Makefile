VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

ANTALYA_IMAGE ?= altinity/clickhouse-server:26.3.13.20001.altinityantalya

.PHONY: build install test test-integration lint fmt vet clickhouse-up clickhouse-down antalya-up antalya-down notices release-build clean

build: ## Build bin/chcli
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/chcli ./cmd/chcli

install: ## Install chcli into GOBIN
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/chcli

test: ## Run unit tests
	go test -race ./...

# Needs the servers started by "make clickhouse-up antalya-up"; tests for a
# server that is not running fail, so start both or unset its variables.
test-integration: ## Run unit and integration tests
	CHCLI_TEST_ADDR=127.0.0.1:19000 CHCLI_TEST_HTTP_ADDR=127.0.0.1:18123 CHCLI_TEST_PASSWORD=chcli-test \
	CHCLI_TEST_TOKEN_ADDR=127.0.0.1:19001 CHCLI_TEST_TOKEN_HTTP_ADDR=127.0.0.1:18124 \
		go test -race -tags integration ./...

clickhouse-up: ## Start a disposable ClickHouse server for integration tests
	docker run -d --rm --name chcli-clickhouse \
		-p 127.0.0.1:19000:9000 -p 127.0.0.1:18123:8123 \
		-e CLICKHOUSE_PASSWORD=chcli-test --ulimit nofile=262144:262144 \
		clickhouse/clickhouse-server:latest

clickhouse-down: ## Stop the disposable ClickHouse server
	docker stop chcli-clickhouse

antalya-up: ## Start a disposable Altinity Antalya server with token authentication
	docker run -d --rm --name chcli-antalya \
		-p 127.0.0.1:19001:9000 -p 127.0.0.1:18124:8123 \
		-e CLICKHOUSE_PASSWORD=chcli-test -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
		--ulimit nofile=262144:262144 \
		-v "$(CURDIR)/testdata/antalya/token-auth.xml:/etc/clickhouse-server/config.d/token-auth.xml:ro" \
		-v "$(CURDIR)/testdata/antalya/init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
		$(ANTALYA_IMAGE)

antalya-down: ## Stop the disposable Antalya server
	docker stop chcli-antalya

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format the code
	gofmt -w cmd internal

vet: ## Run go vet, including integration-tagged files
	go vet -tags integration ./...

notices: ## Regenerate THIRD_PARTY_NOTICES.md after changing dependencies
	scripts/third-party-notices.sh

release-build: ## Build the release archives for every platform into dist/
	scripts/build-release.sh $(VERSION)

clean:
	rm -rf bin dist
