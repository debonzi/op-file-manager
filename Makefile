.PHONY: build test race vet run

build:
	go build -o ./bin/opfm ./cmd/opfm

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

run:
	go run ./cmd/opfm
