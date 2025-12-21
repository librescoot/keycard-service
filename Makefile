.PHONY: build build-arm build-host dist lint test fmt deps run clean

BUILD_DIR := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS) -s -w" -o $(BUILD_DIR)/keycard-service ./cmd/keycard-service

build-arm: build

build-host:
	mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/keycard-service ./cmd/keycard-service

dist: build

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
