.PHONY: build test install logs status

build:
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/status-collector ./cmd/status-collector

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
