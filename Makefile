VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN     := bin/give-me-real-gpu
IMAGE   := give-me-real-gpu:$(VERSION)

.PHONY: build test vet docker up down

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/give-me-real-gpu

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "files need gofmt"; exit 1)

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

up:
	VERSION=$(VERSION) docker compose up -d --build

down:
	docker compose down
