#!/bin/bash
# Builds release archives of the CLI into dist/, with the API bundle embedded
# and the sandbox image pinned to the same version.
#   scripts/build-cli.sh v0.1.0
set -euo pipefail
version=${1:?usage: scripts/build-cli.sh <version>}
root=$(cd "$(dirname "$0")/.." && pwd)
dist="$root/dist"
image="ghcr.io/lucataco/cloudex-base:$version"
rm -rf "$dist" && mkdir -p "$dist"

"$root/scripts/bundle-api.sh" "$version"

cd "$root/go"
ldflags="-s -w -X main.version=$version -X github.com/lucataco/cloudex/go/internal/deploy.DefaultImage=$image"
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os=${target%/*} arch=${target#*/}
  name="cloudex_${os}_${arch}"
  work="$dist/$name"
  mkdir -p "$work"
  exe=cloudex; [ "$os" = windows ] && exe=cloudex.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$work/$exe" ./cmd/cloudex
  cp "$root/LICENSE" "$root/README.md" "$work/"
  if [ "$os" = windows ]; then
    (cd "$work" && zip -q "../$name.zip" "$exe" LICENSE README.md)
  else
    tar -C "$work" -czf "$dist/$name.tar.gz" "$exe" LICENSE README.md
  fi
  rm -rf "$work"
done
cd "$dist" && shasum -a 256 cloudex_* > checksums.txt
ls -l "$dist"
