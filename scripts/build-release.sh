#!/usr/bin/env bash
# Cross-compile forge for every supported platform, stamp the version, and write
# checksums. Used by the release workflow (.github/workflows/release.yml) and
# runnable locally: scripts/build-release.sh v0.1.0
set -euo pipefail

VERSION="${1:-dev}"
PKG="./cmd/forge"
OUTDIR="dist"

# Build matrix — the platforms install.sh knows how to fetch.
PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
)

rm -rf "$OUTDIR"
mkdir -p "$OUTDIR"

for platform in "${PLATFORMS[@]}"; do
  os="${platform%/*}"
  arch="${platform#*/}"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  out="$OUTDIR/forge_${VERSION}_${os}_${arch}${ext}"
  echo "building $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$out" "$PKG"
done

# Checksums over the binaries only (the SHA256SUMS name doesn't match forge_*).
( cd "$OUTDIR" && sha256sum forge_* > "SHA256SUMS.txt" )

echo
echo "artifacts in $OUTDIR:"
ls -la "$OUTDIR"
