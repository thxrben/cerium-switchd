GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/thxrben/cerium-switchd/internal/version.Version=$(VERSION) -X github.com/thxrben/cerium-switchd/internal/version.Date=$(BUILD_DATE)
ARCHES  := amd64 arm64 arm

# Every program is built on its own (reference 1.9): `make switchd`,
# `make swcli`, `make cer-lldpd`, ... PROGRAMS are those installed on a
# switch; rtest is a test tool (test/interop).
PROGRAMS := $(filter-out rtest,$(notdir $(wildcard cmd/*)))

.PHONY: all build test fuzz vet cross image clean $(PROGRAMS) rtest

all: vet test build

build: $(PROGRAMS)

$(PROGRAMS) rtest:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$@ ./cmd/$@

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

# Static programs for all supported architectures: dist/<program>-linux-<arch>.
cross:
	@mkdir -p dist
	for a in $(ARCHES); do for p in $(PROGRAMS); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOARM=7 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$p-linux-$$a ./cmd/$$p || exit 1; \
	done; done

# The cerOS image (docs/os-image.md §6): a signed bundle for updates and a
# complete disk image (DISK GiB). Needs docker. CEROS_SIGNING_KEY signs it
# (default: the development key).
DISK ?= 8
image:
	image/build.sh $(VERSION) $(DISK)

clean:
	rm -rf bin dist build
