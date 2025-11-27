.PHONY: build build-native run clean

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -o bin/keycard-service ./cmd/keycard-service

build-native:
	go build -o bin/keycard-service ./cmd/keycard-service

run:
	go run ./cmd/keycard-service

clean:
	rm -rf bin/
