VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# nodynamic: HEIC/AVIF always decode through the embedded WASM build instead of
# dlopen'ing system libheif/libavif, which keeps the binary fully static.
GOFLAGS = -trimpath -tags nodynamic -ldflags "-s -w -X main.version=$(VERSION)"

trunk:
	CGO_ENABLED=0 go build $(GOFLAGS) -o trunk ./cmd/trunk

test:
	go test -tags nodynamic ./...

.PHONY: trunk test
