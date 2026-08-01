.PHONY: run build pi-arm64 deploy release clean

BINARY   = cove
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
PI_USER ?= isaac
PI_HOST ?= isaac-1.tail2c26ee.ts.net
PI_DIR   = /home/isaac/cove

LDFLAGS  = -ldflags="-s -w -X main.version=$(VERSION)"

run:
	go run ./cmd/cove --root ./test-storage --addr :8080 --password test123

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/cove

pi-arm64:
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY)-pi ./cmd/cove

deploy: pi-arm64
	# cove runs as root (systemd unit User=root), so a non-sudo pkill from an
	# isaac-user SSH session can't touch it — and scp overwriting a binary
	# that's still running/mapped for exec hits ETXTBSY either way. Stopping
	# the unit via systemctl first blocks until the process has actually
	# exited, so the scp below always lands on a free file.
	ssh $(PI_USER)@$(PI_HOST) "mkdir -p $(PI_DIR)/web && sudo systemctl stop cove"
	scp $(BINARY)-pi $(PI_USER)@$(PI_HOST):$(PI_DIR)/cove
	scp web/index.html $(PI_USER)@$(PI_HOST):$(PI_DIR)/web/index.html
	scp web/login.html $(PI_USER)@$(PI_HOST):$(PI_DIR)/web/login.html
	ssh $(PI_USER)@$(PI_HOST) "chmod +x $(PI_DIR)/cove && sudo systemctl start cove"

# Build all release binaries locally (mirrors what CI does)
release-build:
	mkdir -p dist
	GOOS=linux GOARCH=arm64       go build $(LDFLAGS) -o dist/cove-linux-arm64  ./cmd/cove
	GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o dist/cove-linux-arm32  ./cmd/cove
	GOOS=linux GOARCH=amd64       go build $(LDFLAGS) -o dist/cove-linux-amd64  ./cmd/cove
	tar -czf dist/cove-web.tar.gz web/
	cd dist && sha256sum * > checksums.txt
	@echo ""
	@echo "Release binaries in dist/:"
	@ls -lh dist/

# Tag and push a release (triggers GitHub Actions)
# Usage: make release VERSION=v1.0.0
release:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "Usage: make release VERSION=v1.0.0"; exit 1; fi
	git tag $(VERSION)
	git push origin $(VERSION)
	@echo "Release $(VERSION) pushed — GitHub Actions will build and publish it."

clean:
	rm -f $(BINARY) $(BINARY)-pi
	rm -rf dist/
