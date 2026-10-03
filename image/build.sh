#!/bin/bash
# Builds the cerOS image for one version (docs/os-image.md §6):
#   image/build.sh <version> [disk-size-GiB]
# Writes dist/ceros-<version>-amd64.bundle (signed with $CEROS_SIGNING_KEY,
# default: the development key image/keys/dev.key) and, with a disk size,
# dist/ceros-<version>-amd64.img. Needs docker (the ceros-build image is
# built from image/Dockerfile).
set -euo pipefail
cd "$(dirname "$0")/.."
version=$1 disk=${2:-}
key=${CEROS_SIGNING_KEY:-image/keys/dev.key}
keys=${CEROS_TRUSTED_KEYS:-image/keys}
work=build/image-$version
mkdir -p "$work" dist
docker build -q -t ceros-build image >/dev/null
# Every program of the switch (reference 1.9; rtest is a test tool).
mkdir -p "$work/bin" && rm -f "$work/bin/"*
for p in $(ls cmd | grep -v '^rtest$'); do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X github.com/thxrben/cerium-switchd/internal/version.Version=$version -X github.com/thxrben/cerium-switchd/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o "$work/bin/$p" "./cmd/$p"
done
mkdir -p "$work/keys" && rm -f "$work/keys/"*.pub && cp "$keys"/*.pub "$work/keys/"
epoch=$(git log -1 --format=%ct 2>/dev/null || date +%s)
docker run --rm --privileged -e SOURCE_DATE_EPOCH="$epoch" -v "$PWD":/src:ro -v "$PWD/$work":/work ceros-build \
  bash -c "bash /src/image/build-rootfs.sh /work/out '$version' /work/bin /work/keys && chown -R $(id -u):$(id -g) /work"
v=$work/out/verity.json
roothash=$(sed -E 's/.*"roothash":"([0-9a-f]+)".*/\1/' "$v")
off=$(sed -E 's/.*"hash_offset":([0-9]+).*/\1/' "$v")
go run ./cmd/switchd bundle -o "dist/ceros-$version-amd64.bundle" -image "$work/out/rootfs.img" -key "$key" \
  -version "$version" -built "$(date -u -d @"$epoch" +%Y-%m-%dT%H:%M:%SZ)" -roothash "$roothash" -hash-offset "$off"
(cd dist && sha256sum "ceros-$version-amd64.bundle" > "ceros-$version-amd64.bundle.sha256")
echo "dist/ceros-$version-amd64.bundle"
if [ -n "$disk" ]; then
  docker run --rm -v "$PWD":/src:ro -v "$PWD/$work":/work ceros-build \
    bash -c "bash /src/image/make-disk.sh /work/out '$version' /work/disk.img '$disk' && chown $(id -u):$(id -g) /work/disk.img"
  mv "$work/disk.img" "dist/ceros-$version-amd64.img"
  echo "dist/ceros-$version-amd64.img"
fi
