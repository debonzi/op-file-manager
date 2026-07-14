.PHONY: build test race vet run

VERSION ?= dev
BUILD_DIR ?= ./bin

build:
	mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BUILD_DIR)/opfm ./cmd/opfm

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

run:
	go run -ldflags "-X main.version=$(VERSION)" ./cmd/opfm
