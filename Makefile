BINARY := agentd
PKG := ./cmd/agentd

.PHONY: build run tidy vet test proto lint-proto clean

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

# Regenerate Go stubs from the owned protos (needs buf + protoc-gen-go[-grpc]).
proto:
	buf generate

lint-proto:
	buf lint

clean:
	rm -rf bin/
