GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/thxrben/cerium-switchd/lib/platform/version.Version=$(VERSION) -X github.com/thxrben/cerium-switchd/lib/platform/version.Date=$(BUILD_DATE)
ARCHES  := amd64 arm64 arm

# Every program is built on its own (reference 1.9): `make switchd`,
# `make swcli`, `make cer-lldpd`, ... PROGRAMS are those installed on a
# switch; rtest is a test tool (test/interop).
PROGRAMS := $(filter-out rtest,$(notdir $(wildcard apps/*)))

# Every module of the repository (lib/*, apps/*, and the root with docs/test).
MODULES := . $(patsubst %/go.mod,%,$(shell find lib apps -name go.mod | sort))

.PHONY: all build test interop-bgp fuzz vet cross image clean $(PROGRAMS) rtest

all: vet test build

build: $(PROGRAMS)

$(PROGRAMS) rtest:
	@mkdir -p bin
	cd apps/$@ && CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(CURDIR)/bin/$@ .

test:
	@for m in $(MODULES); do (cd $$m && $(GO) test ./...) || exit 1; done

# BGP against the GoBGP server, in one process (a module of its own: GoBGP's
# server never becomes a dependency of cerOS).
interop-bgp:
	cd test/interop/gobgp && $(GO) test ./...

vet:
	@for m in $(MODULES); do (cd $$m && $(GO) vet ./...) || exit 1; done
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt needed"; exit 1)

# Short fuzzing pass over all fuzz targets (FUZZTIME per target).
FUZZTIME ?= 20s
fuzz:
	@for pkg in $$($(GO) list $(addsuffix /...,$(MODULES)) 2>/dev/null); do \
	  for f in $$($(GO) test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz'); do \
	    echo "== $$pkg $$f"; $(GO) test $$pkg -run xxx -fuzz "^$$f$$" -fuzztime $(FUZZTIME) || exit 1; \
	  done; \
	done

# Static programs for all supported architectures: dist/<program>-linux-<arch>.
cross:
	@mkdir -p dist
	for a in $(ARCHES); do for p in $(PROGRAMS); do \
	  cd apps/$$p && CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOARM=7 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(CURDIR)/dist/$$p-linux-$$a . || exit 1; \
	done; done

# The cerOS image (docs/os-image.md §6): a signed bundle for updates and a
# complete disk image (DISK GiB). Needs docker. CEROS_SIGNING_KEY signs it
# (default: the development key).
DISK ?= 8
image:
	image/build.sh $(VERSION) $(DISK)

clean:
	rm -rf bin dist build
