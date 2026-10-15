#!/bin/sh
# Install tego from its GitHub releases:
#
#   curl -fsSL https://raw.githubusercontent.com/akbxr/tego/main/install.sh | sh
#
# TEGO_VERSION   a release tag such as v0.2.0 (default: the latest)
# TEGO_DIR       where to put the binary (default: ~/.local/bin)
# TEGO_BASE_URL  download from a mirror instead of GitHub
set -eu

REPO="akbxr/tego"
DIR="${TEGO_DIR:-$HOME/.local/bin}"
VERSION="${TEGO_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() { printf 'tego install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported OS $(uname -s); tego runs on macOS and Linux" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

if [ -n "${TEGO_BASE_URL:-}" ]; then
  base="$TEGO_BASE_URL"
elif [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  die "needs curl or wget"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
asset="tego_${os}_${arch}.tar.gz"

say "Downloading $asset ($VERSION)..."
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

want="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || die "$asset is not in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $asset"

tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$DIR"
mv "$tmp/tego_${os}_${arch}/tego" "$DIR/tego"
chmod +x "$DIR/tego"
[ "$os" = darwin ] && xattr -d com.apple.quarantine "$DIR/tego" 2>/dev/null || true

say "Installed $("$DIR/tego" --version) to $DIR/tego"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) say "Add $DIR to your PATH, for example: echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.zshrc" ;;
esac
