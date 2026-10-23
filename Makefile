GO_IMAGE ?= golang:1.27
IMAGE    ?= gateway:dev

# Go is not installed locally, so every go command runs inside the official image
# as the current user (files stay owned by you) with host-mounted caches.
GO = docker run --rm \
	--user $(shell id -u):$(shell id -g) \
	-e HOME=/tmp -e GOCACHE=/gocache -e GOMODCACHE=/gomod \
	-v $(CURDIR):/src -w /src \
	-v $(HOME)/.cache/go-build:/gocache \
	-v $(HOME)/go/pkg/mod:/gomod \
	$(GO_IMAGE) go

.PHONY: caches fmt vet staticcheck deadcode test check cover tidy build docker-build docker-run e2e

caches:
	@mkdir -p $(HOME)/.cache/go-build $(HOME)/go/pkg/mod

fmt: caches
	$(GO) fmt ./...

vet: caches
	$(GO) vet ./...

staticcheck: caches
	$(GO) tool staticcheck ./...

# deadcode prints findings and still exits 0, so an empty report is the pass condition.
deadcode: caches
	@out=$$($(GO) tool deadcode -test ./...); \
	if [ -n "$$out" ]; then printf '%s\n' "$$out"; exit 1; fi

test: caches
	$(GO) test -race -count=1 ./...

check: build vet staticcheck deadcode test

cover: caches
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

tidy: caches
	$(GO) mod tidy

build: caches
	$(GO) build -o bin/gateway ./cmd/gateway

docker-build:
	docker build -t $(IMAGE) .

docker-run: docker-build
	docker run --rm -p 8080:8080 \
		--add-host=host.docker.internal:host-gateway \
		-v $(CURDIR)/configs/gateway.yaml:/etc/gateway/config.yaml:ro \
		$(IMAGE)

# Three containers: client, gateway, and the internal mock service.
e2e:
	docker compose -f test/e2e/compose.yaml up --build --abort-on-container-exit --exit-code-from client; \
	code=$$?; \
	docker compose -f test/e2e/compose.yaml down -v; \
	exit $$code
