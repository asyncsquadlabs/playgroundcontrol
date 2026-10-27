SHELL := /bin/sh
VERSION ?= 1.0.0
GO ?= go
GO_WINRES ?= go-winres
GOVULNCHECK ?= govulncheck
GOSEC ?= gosec
STATICCHECK ?= staticcheck
DIST := dist/PlaygroundControl.exe
LDFLAGS := -s -w -H=windowsgui -X github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo.Version=$(VERSION)
# Optional signing hook. Put the certificate password in the environment and
# let the shell expand it; never commit signing material.
# SIGN_CMD='signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 /f cert.pfx /p "$CERT_PASSWORD"'
SIGN_CMD ?=

.PHONY: all mod-verify test race vet check security windows-check resources build sign clean

all: check build

mod-verify:
	$(GO) mod verify

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: mod-verify vet test
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -d $$(gofmt -l cmd internal); exit 1)
	node --check internal/server/ui/app.js
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) vet ./...

security:
	$(GOVULNCHECK) ./...
	$(GOSEC) -quiet ./...
	$(STATICCHECK) ./...

windows-check:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build ./...

resources:
	$(GO_WINRES) make --in winres/winres.json --arch amd64 --out cmd/playgroundcontrol/rsrc --product-version $(VERSION) --file-version $(VERSION)

build:
	mkdir -p dist
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false \
		-ldflags "$(LDFLAGS)" -o $(DIST) ./cmd/playgroundcontrol
	sha256sum $(DIST) > $(DIST).sha256
	@ls -lh $(DIST)

sign: build
	@test -n "$(SIGN_CMD)" || (echo "Set SIGN_CMD to an Authenticode signing command." >&2; exit 1)
	$(SIGN_CMD) $(DIST)
	sha256sum $(DIST) > $(DIST).sha256

clean:
	rm -rf dist
