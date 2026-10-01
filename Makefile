GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X mclag/internal/version.Version=$(VERSION) -X mclag/internal/version.Date=$(BUILD_DATE)
ARCHES  := amd64 arm64 arm

.PHONY: all build test fuzz vet cross image clean

all: vet test build

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/switchd ./cmd/switchd
	ln -sf switchd bin/swcli

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

# Static binaries for all supported architectures (swcli is a symlink to switchd).
cross:
	@mkdir -p dist
	for a in $(ARCHES); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOARM=7 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/switchd-linux-$$a ./cmd/switchd || exit 1; \
	done

# The cerOS image (docs/os-image.md §6): a signed bundle for updates and a
# complete disk image (DISK GiB). Needs docker. CEROS_SIGNING_KEY signs it
# (default: the development key).
DISK ?= 8
image:
	image/build.sh $(VERSION) $(DISK)

clean:
	rm -rf bin dist build
