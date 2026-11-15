GO ?= go
GOFMT ?= gofmt

.PHONY: build test check install clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/superwhisper-sonos .

test:
	$(GO) test -race ./...

check:
	$(GO) vet ./...
	@test -z "$$($(GOFMT) -l .)" || { echo 'Run gofmt -w .'; exit 1; }

install: build
	bash scripts/install.sh

clean:
	rm -f bin/superwhisper-sonos
