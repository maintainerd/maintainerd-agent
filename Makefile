BINARY := agentd
PKG := ./cmd/agentd

# --- release ---
# The binary installs on a host as the service's own name; `agentd` is fine in
# bin/ and far too generic in /usr/local/bin.
RELEASE_BINARY := maintainerd-agent
DIST := dist
# `git describe` locally, the tag in CI (release.yml passes VERSION).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# -X stamps config.AppVersion at LINK TIME: a version an operator can set is a
# version that can disagree with the binary it describes. -s -w drop the symbol
# table and DWARF (nothing debugs a fleet host with a local gdb).
LDFLAGS := -s -w -X github.com/maintainerd/agent/internal/platform/config.AppVersion=$(VERSION)
# linux only, and CGO_ENABLED=0 on purpose: the agent is installed on hosts it
# does not choose, so one static binary must run on glibc and musl alike.
RELEASE_ARCHES := amd64 arm64

.PHONY: build run tidy vet test proto lint-proto clean build-release checksums

build:
	go build -o bin/$(BINARY) $(PKG)

run:
	go run $(PKG)

tidy:
	go mod tidy

vet:
	go vet ./...

test:
	go test ./...

# One tarball per platform, each carrying what an operator needs to install the
# agent under systemd without cloning anything: the binary, the unit, the
# environment template, the licence and the README.
build-release:
	rm -rf $(DIST)
	mkdir -p $(DIST)
	@for arch in $(RELEASE_ARCHES); do \
	  set -e; \
	  name="$(RELEASE_BINARY)_$(VERSION)_linux_$$arch"; \
	  stage="$(DIST)/$$name"; \
	  mkdir -p "$$stage"; \
	  echo "building $$name"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$$stage/$(RELEASE_BINARY)" $(PKG); \
	  cp packaging/systemd/maintainerd-agent.service packaging/systemd/agent.env.example README.md "$$stage/"; \
	  if [ -f LICENSE ]; then cp LICENSE "$$stage/"; \
	  else echo "WARNING: no LICENSE in the repo root — $$name ships without one"; fi; \
	  tar -czf "$$stage.tar.gz" -C "$(DIST)" "$$name"; \
	  rm -rf "$$stage"; \
	done

# Covers every file that gets attached to the release. The install path in
# README.md verifies against it before unpacking as root, so this is a gate, not
# a courtesy.
checksums:
	cd $(DIST) && sha256sum *.tar.gz > SHA256SUMS

# Regenerate Go stubs from the owned protos (needs buf + protoc-gen-go[-grpc]).
proto:
	buf generate

lint-proto:
	buf lint

clean:
	rm -rf bin/
