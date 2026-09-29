GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X mclag/internal/version.Version=$(VERSION)
CMDS    := switchd swcli
ARCHES  := amd64 arm64 arm

.PHONY: all build test fuzz vet cross clean

all: vet test build

build:
	@mkdir -p bin
	for c in $(CMDS); do CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$$c ./cmd/$$c || exit 1; done

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt needed"; exit 1)

# Short fuzzing pass over all fuzz targets (FUZZTIME per target).
FUZZTIME ?= 20s
fuzz:
	@for pkg in $$($(GO) list ./...); do \
	  for f in $$($(GO) test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz'); do \
	    echo "== $$pkg $$f"; $(GO) test $$pkg -run xxx -fuzz "^$$f$$" -fuzztime $(FUZZTIME) || exit 1; \
	  done; \
	done

# Static binaries for all supported architectures.
cross:
	@mkdir -p dist
	for a in $(ARCHES); do for c in $(CMDS); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOARM=7 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$c-linux-$$a ./cmd/$$c || exit 1; \
	done; done

clean:
	rm -rf bin dist
