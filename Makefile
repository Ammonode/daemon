VERSION ?= develop
LDFLAGS := -s -w -X github.com/ammonode/daemon/internal/system.Version=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o build/ammonode-daemon ./cmd/ammonode-daemon

build-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/ammonode-daemon_linux_amd64 ./cmd/ammonode-daemon
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/ammonode-daemon_linux_arm64 ./cmd/ammonode-daemon

test:
	go test ./...

lint:
	go vet ./...

.PHONY: build build-all test lint
