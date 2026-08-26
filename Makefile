.PHONY: build build-linux test install logs status

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/status-collector ./cmd/status-collector

# Cross-compile for the NAS. Pure-Go SQLite means no toolchain juggling.
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/status-collector-linux-amd64 ./cmd/status-collector
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/status-collector-linux-arm64 ./cmd/status-collector

test:
	go test ./...

install: build
	sudo install -m 0755 bin/status-collector /usr/local/bin/status-collector
	sudo systemctl daemon-reload
	sudo systemctl restart ol1n-status

logs:
	journalctl -u ol1n-status -f

status:
	systemctl status ol1n-status --no-pager -l
