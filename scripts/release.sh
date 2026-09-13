#!/bin/sh
set -eu
version=${1:?usage: scripts/release.sh VERSION}
mkdir -p dist
for target in linux/amd64 linux/arm64; do
  os=${target%/*}; arch=${target#*/}; name="minicrond-${version}-${os}-${arch}"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version -X main.commit=$(git rev-parse --short HEAD)" -o "dist/$name/minicrond" ./cmd/minicrond
  cp LICENSE README.md "dist/$name/"
  tar -C dist -czf "dist/$name.tar.gz" "$name"
  rm -rf "dist/$name"
done
(cd dist && sha256sum ./*.tar.gz > sha256sums.txt)
if command -v cosign >/dev/null 2>&1; then cosign sign-blob --yes --output-signature dist/sha256sums.txt.sig dist/sha256sums.txt; else echo 'cosign not found; artifacts are checksummed but unsigned' >&2; exit 1; fi
