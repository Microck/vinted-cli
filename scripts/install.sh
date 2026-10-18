#!/bin/sh
# Installs the latest vinted release binary into ~/.local/bin (or $VINTED_INSTALL_DIR).
set -eu
repo="Microck/vinted-cli"
dir="${VINTED_INSTALL_DIR:-$HOME/.local/bin}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported arch: $arch" >&2; exit 1 ;; esac
tag=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
[ -n "$tag" ] || { echo "could not find the latest release" >&2; exit 1; }
version=${tag#v}
asset="vinted_${version}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://github.com/$repo/releases/download/$tag/$asset" -o "$tmp/$asset"
curl -fsSL "https://github.com/$repo/releases/download/$tag/checksums.txt" -o "$tmp/checksums.txt"
# macOS ships shasum, most Linux distributions ship sha256sum.
if command -v sha256sum >/dev/null 2>&1; then checksum_cmd="sha256sum"; else checksum_cmd="shasum -a 256"; fi
(cd "$tmp" && grep " $asset\$" checksums.txt | $checksum_cmd -c - >/dev/null)
tar -xzf "$tmp/$asset" -C "$tmp" vinted
mkdir -p "$dir"
install -m 0755 "$tmp/vinted" "$dir/vinted"
echo "installed vinted $version to $dir/vinted"
case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH" ;; esac
