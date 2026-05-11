.PHONY: build run clean tidy fmt lint test

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
	go test ./...
