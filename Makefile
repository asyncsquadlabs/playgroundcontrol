SHELL := /bin/sh
VERSION ?= 1.4.0
GO ?= go
GO_WINRES ?= go-winres
GOVULNCHECK ?= govulncheck
GOSEC ?= gosec
STATICCHECK ?= staticcheck
DIST := dist/PlaygroundControl.exe
PACKAGE_DIR := dist/PlaygroundControl-$(VERSION)-win-x64
PACKAGE_ZIP := dist/PlaygroundControl-$(VERSION)-win-x64.zip
LDFLAGS := -s -w -H=windowsgui -X github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo.Version=$(VERSION)
# Optional signing hook. Put the certificate password in the environment and
# let the shell expand it; never commit signing material.
# SIGN_CMD='signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 /f cert.pfx /p "$CERT_PASSWORD"'
SIGN_CMD ?=

.PHONY: all mod-verify test race vet check security windows-check resources build package release sign clean

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
	@(cd dist && sha256sum PlaygroundControl.exe > PlaygroundControl.exe.sha256)
	@ls -lh $(DIST)

package: build
	rm -rf "$(PACKAGE_DIR)"
	mkdir -p "$(PACKAGE_DIR)"
	cp "$(DIST)" "$(PACKAGE_DIR)/PlaygroundControl.exe"
	cp README.md SECURITY.md LICENSE THIRD_PARTY_NOTICES.md TESTING.html "$(PACKAGE_DIR)/"
	@(cd "$(PACKAGE_DIR)" && sha256sum PlaygroundControl.exe > PlaygroundControl.exe.sha256)
	@(cd dist && rm -f "PlaygroundControl-$(VERSION)-win-x64.zip" && zip -qr "PlaygroundControl-$(VERSION)-win-x64.zip" "PlaygroundControl-$(VERSION)-win-x64")
	@(cd dist && sha256sum "PlaygroundControl-$(VERSION)-win-x64.zip" > "PlaygroundControl-$(VERSION)-win-x64.zip.sha256")
	@ls -lh "$(PACKAGE_ZIP)"

release: check package

sign: build
	@test -n "$(SIGN_CMD)" || (echo "Set SIGN_CMD to an Authenticode signing command." >&2; exit 1)
	$(SIGN_CMD) $(DIST)
	@(cd dist && sha256sum PlaygroundControl.exe > PlaygroundControl.exe.sha256)

clean:
	rm -rf dist
