#!/usr/bin/env bash
# forge installer — downloads a prebuilt binary from the latest GitHub Release
# and installs it on your PATH. No Go toolchain required.
#
#   curl -fsSL https://raw.githubusercontent.com/forgehubproject/forge/main/install.sh | bash
#
# Pin a version with FORGE_VERSION, or override the install dir with FORGE_INSTALL_DIR:
#   FORGE_VERSION=v0.1.0 bash install.sh
set -euo pipefail

REPO="forgehubproject/forge"
BINARY_NAME="forge"

# ── platform detection ────────────────────────────────────────────────────────

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Linux)               GOOS="linux" ;;
  Darwin)              GOOS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) GOOS="windows" ;;
  *)
    echo "error: unsupported OS: $OS" >&2
    echo "       forge ships binaries for Linux, macOS, and Windows (via Git Bash/MSYS/WSL)." >&2
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64)   GOARCH="amd64" ;;
  aarch64|arm64)  GOARCH="arm64" ;;
  *)
    echo "error: no prebuilt binary for architecture: $ARCH" >&2
    echo "       Prebuilt binaries cover amd64 and arm64. To build from source:" >&2
    echo "         git clone https://github.com/$REPO && cd forge && go build -o forge ./cmd/forge" >&2
    exit 1
    ;;
esac

# Windows binaries are only built for amd64.
if [ "$GOOS" = "windows" ] && [ "$GOARCH" != "amd64" ]; then
  echo "error: Windows binaries are published for amd64 only." >&2
  exit 1
fi

EXT=""
[ "$GOOS" = "windows" ] && { EXT=".exe"; BINARY_NAME="forge.exe"; }

echo "Platform: $GOOS/$GOARCH"

# ── resolve the version ───────────────────────────────────────────────────────

TAG="${FORGE_VERSION:-}"
if [ -z "$TAG" ]; then
  echo "Resolving latest release..."
  TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
fi
if [ -z "$TAG" ]; then
  echo "error: could not determine the latest release tag." >&2
  echo "       Set one explicitly, e.g. FORGE_VERSION=v0.1.0 bash install.sh" >&2
  exit 1
fi
echo "Version:  $TAG"

ASSET="forge_${TAG}_${GOOS}_${GOARCH}${EXT}"
BASE="https://github.com/$REPO/releases/download/$TAG"

# ── download + verify ─────────────────────────────────────────────────────────

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading $ASSET..."
if ! curl -fSL "$BASE/$ASSET" -o "$TMP/$BINARY_NAME"; then
  echo "error: download failed for $BASE/$ASSET" >&2
  echo "       Check that release $TAG publishes an asset for $GOOS/$GOARCH." >&2
  exit 1
fi

# Verify the checksum when the release ships SHA256SUMS.txt and sha256sum exists.
if command -v sha256sum >/dev/null 2>&1 && curl -fsSL "$BASE/SHA256SUMS.txt" -o "$TMP/SHA256SUMS.txt" 2>/dev/null; then
  EXPECTED="$(grep " ${ASSET}\$" "$TMP/SHA256SUMS.txt" | awk '{print $1}')"
  if [ -n "$EXPECTED" ]; then
    ACTUAL="$(sha256sum "$TMP/$BINARY_NAME" | awk '{print $1}')"
    if [ "$EXPECTED" != "$ACTUAL" ]; then
      echo "error: checksum mismatch for $ASSET" >&2
      echo "       expected $EXPECTED" >&2
      echo "       got      $ACTUAL" >&2
      exit 1
    fi
    echo "Checksum: verified"
  fi
fi

chmod +x "$TMP/$BINARY_NAME"

# ── choose an install dir ─────────────────────────────────────────────────────

if [ -n "${FORGE_INSTALL_DIR:-}" ]; then
  INSTALL_DIR="$FORGE_INSTALL_DIR"
elif [ "$GOOS" = "windows" ]; then
  INSTALL_DIR="$HOME/bin"
elif [ "$GOOS" = "darwin" ] && command -v brew >/dev/null 2>&1; then
  INSTALL_DIR="$(brew --prefix)/bin"
else
  INSTALL_DIR="/usr/local/bin"
fi
mkdir -p "$INSTALL_DIR" 2>/dev/null || true

DEST="$INSTALL_DIR/$BINARY_NAME"
if [ -w "$INSTALL_DIR" ]; then
  install -m 755 "$TMP/$BINARY_NAME" "$DEST"
else
  echo "Installing to $DEST (requires sudo)..."
  sudo install -m 755 "$TMP/$BINARY_NAME" "$DEST"
fi

echo "Installed: $DEST"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: add $INSTALL_DIR to your PATH to run 'forge' from any shell." ;;
esac

"$DEST" --version 2>/dev/null || true
echo "Done — run 'forge --help' to get started."
