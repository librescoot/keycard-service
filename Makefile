.PHONY: build build-arm build-host build-amd64 dist lint test fmt deps run clean

BUILD_DIR := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

build: build-arm

build-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/keycard-service ./cmd/keycard-service

build-host:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/keycard-service ./cmd/keycard-service

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/keycard-service ./cmd/keycard-service

dist:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS) -s -w" -o $(BUILD_DIR)/keycard-service ./cmd/keycard-service

lint:
	golangci-lint run

test:
	go test -v ./...

fmt:
	go fmt ./...

deps:
	go mod download && go mod tidy

run:
	go run ./cmd/keycard-service

clean:
	rm -rf $(BUILD_DIR)/
