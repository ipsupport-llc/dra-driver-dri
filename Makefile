VERSION ?= $(shell git describe --tags --always --dirty)
IMAGE   ?= ghcr.io/ipsupport-llc/dra-driver-dri

.PHONY: test lint build image
test:
	go vet ./...
	go test -race ./...
lint:
	golangci-lint run ./...
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/dra-driver-dri ./cmd/dra-driver-dri
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .
