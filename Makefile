.PHONY: build test lint tidy

build:
	go build ./...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

tidy:
	go mod tidy
