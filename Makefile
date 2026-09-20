# Reproducible build and test entry points.
#
# Node is a contributor build dependency: it compiles the dashboard, which is then embedded
# into the Go binary. End users need only the resulting executable.

BINARY   := codexrelay
VERSION  ?= dev
LDFLAGS  := -s -w -X main.Version=$(VERSION)
DIST     := internal/httpapi/dist

.PHONY: all web build test vet check clean cross dist package-clean

all: check

## web: compile the dashboard into the embedded asset directory
web:
	cd web && npm ci && npm run build

## build: compile the dashboard, then the executable for this machine
build: web
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/codexrelay
	cp scripts/relaypool bin/relaypool
	chmod +x bin/relaypool

## test: run the Go test suite
test:
	go test ./...

vet:
	go vet ./...

## check: what CI runs
check: vet test
	go build ./...

## cross: prove every supported target compiles. Compiling is NOT evidence that
## installation or OS credential storage works on that platform; see docs/compatibility.md.
cross: web
	@set -e; for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o bin/$(BINARY)-$$os-$$arch$$ext ./cmd/codexrelay; \
	done
	@ls -l bin/

clean:
	rm -rf bin $(DIST)/assets $(DIST)/index.html

## dist: build every supported target and package it for distribution.
##
## What this produces and what it does NOT produce: reproducible, checksummed archives
## containing the executable, the README and the licence. It does NOT produce a signed or
## notarised macOS package, an MSI, a .deb or an .rpm, and it does not register a background
## service. Those need signing identities and per-platform installer tooling that this build
## has never had access to. See docs/packaging.md for exactly what is missing and why.
##
## Nothing here publishes. `dist` writes to dist/ and stops.
dist: web
	@set -e; 	rm -rf dist; mkdir -p dist; 	for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do 		os=$${t%/*}; arch=$${t#*/}; ext=""; 		if [ "$$os" = "windows" ]; then ext=".exe"; fi; 		stage="dist/stage/$(BINARY)-$(VERSION)-$$os-$$arch"; 		mkdir -p "$$stage"; 		echo "building $$os/$$arch"; 		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch SOURCE_DATE_EPOCH=0 			go build -trimpath -ldflags '$(LDFLAGS)' -o "$$stage/$(BINARY)$$ext" ./cmd/codexrelay; 		if [ "$$os" = "windows" ]; then cp scripts/relaypool.cmd "$$stage/relaypool.cmd"; else cp scripts/relaypool "$$stage/relaypool"; chmod +x "$$stage/relaypool"; fi; 		cp README.md LICENSE "$$stage/" 2>/dev/null || cp README.md "$$stage/"; 		cp docs/setup-and-recovery.md "$$stage/SETUP.md"; 		if [ "$$os" = "windows" ]; then 			( cd dist/stage && zip -q -r "../$(BINARY)-$(VERSION)-$$os-$$arch.zip" 				"$(BINARY)-$(VERSION)-$$os-$$arch" ); 		else 			tar -C dist/stage --numeric-owner --owner=0 --group=0 				-czf "dist/$(BINARY)-$(VERSION)-$$os-$$arch.tar.gz" 				"$(BINARY)-$(VERSION)-$$os-$$arch"; 		fi; 	done; 	rm -rf dist/stage; 	( cd dist && shasum -a 256 * > SHA256SUMS ); 	echo; ls -lh dist/; echo; cat dist/SHA256SUMS

package-clean:
	rm -rf dist
