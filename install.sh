#!/bin/sh
# flex installer. Usage:
#   curl -fsSL https://raw.githubusercontent.com/PaulOh5/gpu-private-cloud-with-gstack/main/install.sh | sh
#   FLEX_VERSION=v0.1.0 ./install.sh        # pin a version
#   FLEX_BINDIR=~/.local/bin ./install.sh   # choose install dir
#
# Downloads the matching release binary from GitHub Releases and installs it.
# No GPU or build toolchain required (flex is a single static binary).
set -eu

REPO="PaulOh5/gpu-private-cloud-with-gstack"

# --- detect platform ---
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) echo "flex: unsupported OS: $os" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "flex: unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ "$os" = "darwin" ] && [ "$arch" = "amd64" ]; then
  echo "flex: no macOS Intel build is published; use an arm64 Mac or build from source" >&2
  exit 1
fi

# --- resolve version ---
tag="${FLEX_VERSION:-}"
if [ -z "$tag" ]; then
  tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name":' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
fi
if [ -z "$tag" ]; then
  echo "flex: could not determine latest version; set FLEX_VERSION (e.g. v0.1.0)" >&2
  exit 1
fi
ver="${tag#v}"

# --- download + extract ---
asset="flex_${ver}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$tag/$asset"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "flex: downloading $asset ($tag)"
curl -fsSL "$url" -o "$tmp/$asset" || { echo "flex: download failed: $url" >&2; exit 1; }
tar -xzf "$tmp/$asset" -C "$tmp"

# --- install ---
bindir="${FLEX_BINDIR:-/usr/local/bin}"
if [ ! -d "$bindir" ] || [ ! -w "$bindir" ]; then
  # Fall back to a user-writable dir if the default needs root.
  if [ "${FLEX_BINDIR:-}" = "" ]; then
    bindir="$HOME/.local/bin"
    mkdir -p "$bindir"
  else
    echo "flex: $bindir is not writable" >&2; exit 1
  fi
fi

install -m 0755 "$tmp/flex" "$bindir/flex"
echo "flex: installed to $bindir/flex"

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) echo "flex: add $bindir to your PATH to use 'flex'" ;;
esac

"$bindir/flex" version
