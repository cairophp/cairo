#!/usr/bin/env bash
# Installs the cairo release binary for this machine:
#   curl -fsSL https://raw.githubusercontent.com/cairophp/cairo/main/install.sh | bash
# CAIRO_VERSION picks a release (default: the latest), CAIRO_INSTALL_DIR where
# it goes (default: /usr/local/bin). The download is checked against the
# release's SHA256SUMS before anything is installed.
set -euo pipefail

version="${CAIRO_VERSION:-latest}"
dir="${CAIRO_INSTALL_DIR:-/usr/local/bin}"

if [ "$(uname -s)" != "Linux" ]; then
    echo "cairo runs on Linux only; on macOS and Windows use the Docker image ghcr.io/cairophp/cairo" >&2
    exit 1
fi
case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "cairo has no release binary for $(uname -m)" >&2; exit 1 ;;
esac

releases="https://github.com/cairophp/cairo/releases"
if [ "$version" = "latest" ]; then
    url="$releases/latest/download"
else
    url="$releases/download/$version"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bin="cairo-linux-$arch"
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$bin" "$url/$bin"
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/SHA256SUMS" "$url/SHA256SUMS"
(cd "$tmp" && sha256sum --check --ignore-missing --status SHA256SUMS) || {
    echo "the checksum of $bin does not match SHA256SUMS; nothing was installed" >&2
    exit 1
}
# --ignore-missing also passes when the file is not listed at all.
grep -q " $bin\$" "$tmp/SHA256SUMS" || {
    echo "SHA256SUMS does not list $bin; nothing was installed" >&2
    exit 1
}

if [ -w "$dir" ]; then
    install -m 755 "$tmp/$bin" "$dir/cairo"
else
    echo "installing to $dir needs root"
    sudo install -m 755 "$tmp/$bin" "$dir/cairo"
fi
"$dir/cairo" version
