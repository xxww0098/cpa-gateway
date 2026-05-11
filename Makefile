.PHONY: build run clean tidy fmt lint test test-verbose test-race

BINARY=cpa-gateway
PORT=8888

build:
	go build -o $(BINARY) .

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY)
	go clean

tidy:
	go mod tidy

fmt:
	go fmt ./...

lint:
	golangci-lint run

test:
	go test ./... -count=1 -timeout 30s

test-verbose:
	go test -v ./...

test-race:
	go test -race ./... -count=1
