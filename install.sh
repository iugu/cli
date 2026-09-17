#!/bin/sh
# Installs the latest (or IUGU_VERSION) release of the iugu CLI from GitHub Releases, verifying the
# archive against the signed checksums.txt (cosign verification when cosign is on PATH).
#   curl -fsSL https://raw.githubusercontent.com/iugu/cli/main/install.sh | sh
# Env: IUGU_VERSION (e.g. 1.2.3), IUGU_INSTALL_DIR (default /usr/local/bin or ~/.local/bin), IUGU_REPO.
set -eu

REPO="${IUGU_REPO:-iugu/cli}"
API="https://api.github.com/repos/${REPO}/releases"
DL="https://github.com/${REPO}/releases/download"

need() { command -v "$1" >/dev/null 2>&1 || { echo "install.sh: $1 is required" >&2; exit 1; }; }
need curl
need tar

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "install.sh: unsupported architecture $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "install.sh: unsupported OS $os (use Scoop or the zip on Windows)" >&2; exit 1 ;;
esac

version="${IUGU_VERSION:-}"
if [ -z "$version" ]; then
  version=$(curl -fsSL "${API}/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$version" ] || { echo "install.sh: could not resolve the latest version" >&2; exit 1; }
fi
tag="v${version#v}"
version="${version#v}"

archive="iugu_${version}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading iugu ${version} (${os}/${arch})…"
curl -fsSL -o "${tmp}/${archive}" "${DL}/${tag}/${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${DL}/${tag}/checksums.txt"

if command -v cosign >/dev/null 2>&1; then
  curl -fsSL -o "${tmp}/checksums.txt.sig" "${DL}/${tag}/checksums.txt.sig"
  curl -fsSL -o "${tmp}/checksums.txt.pem" "${DL}/${tag}/checksums.txt.pem"
  cosign verify-blob --certificate "${tmp}/checksums.txt.pem" --signature "${tmp}/checksums.txt.sig" \
    --certificate-identity-regexp "github.com/${REPO}" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "${tmp}/checksums.txt" >/dev/null
  echo "checksums.txt signature verified (cosign)"
else
  echo "cosign not found: skipping signature verification (checksum still enforced)" >&2
fi

expected=$(grep " ${archive}\$" "${tmp}/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || { echo "install.sh: ${archive} not in checksums.txt" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${tmp}/${archive}" | awk '{print $1}')
else
  actual=$(shasum -a 256 "${tmp}/${archive}" | awk '{print $1}')
fi
[ "$expected" = "$actual" ] || { echo "install.sh: checksum mismatch for ${archive}" >&2; exit 1; }

tar -xzf "${tmp}/${archive}" -C "$tmp" iugu

dir="${IUGU_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="${HOME}/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "${tmp}/iugu" "${dir}/iugu"
echo "Installed ${dir}/iugu"
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) echo "Add ${dir} to your PATH." ;;
esac
"${dir}/iugu" --version
