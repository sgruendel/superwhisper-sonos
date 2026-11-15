GO ?= go
GOFMT ?= gofmt

.PHONY: build generate test check vuln install uninstall clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/superwhisper-sonos .

# Regenerates the String methods of the enum types after their constants change.
generate:
	$(GO) generate ./...

test:
	$(GO) test -race ./...

check:
	$(GO) vet ./...
	@test -z "$$($(GOFMT) -l .)" || { echo 'Run gofmt -w .'; exit 1; }
	$(GO) mod tidy -diff
	$(GO) tool staticcheck ./...

# Needs network access to download the Go vulnerability database.
vuln:
	$(GO) tool govulncheck ./...

install: build
	bash scripts/install.sh

uninstall:
	bash scripts/uninstall.sh

clean:
	rm -f bin/superwhisper-sonos
