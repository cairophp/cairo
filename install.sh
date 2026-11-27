#!/usr/bin/env bash
# Installs the cairo release binary for this machine:
#   curl -fsSL https://raw.githubusercontent.com/cairophp/cairo/main/install.sh | bash
# CAIRO_VERSION picks a release (default: the latest), CAIRO_PHP the PHP
# version inside it, such as 8.4 (default: the newest the release has), and
# CAIRO_INSTALL_DIR where it goes (default: /usr/local/bin), and CAIRO_LIBC=gnu
# the glibc binary, which loads extensions, with the kit to build them
# unpacked in /opt/cairo. The downloads are checked against the release's
# SHA256SUMS before anything is installed.
set -euo pipefail

version="${CAIRO_VERSION:-latest}"
php="${CAIRO_PHP:-}"
dir="${CAIRO_INSTALL_DIR:-/usr/local/bin}"
case "${CAIRO_LIBC:-}" in
    "") libc="" ;;
    gnu) libc="-gnu" ;;
    *) echo "CAIRO_LIBC is gnu or unset, not $CAIRO_LIBC" >&2; exit 1 ;;
esac
case "$php" in
    "" | [0-9].[0-9] | [0-9].[0-9][0-9]) ;;
    *) echo "CAIRO_PHP is a PHP version such as 8.4, not $php" >&2; exit 1 ;;
esac

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
bin="cairo${php:+-php$php}-linux-$arch$libc"
files="$bin"
if [ -n "$libc" ]; then files="$bin $bin-dev.tar.gz"; fi
for file in $files; do
    curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$file" "$url/$file" || {
        echo "release $version has no $file${php:+ (PHP $php)}; nothing was installed" >&2
        exit 1
    }
done
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/SHA256SUMS" "$url/SHA256SUMS"
(cd "$tmp" && sha256sum --check --ignore-missing --status SHA256SUMS) || {
    echo "a checksum of $files does not match SHA256SUMS; nothing was installed" >&2
    exit 1
}
# --ignore-missing also passes when the file is not listed at all.
for file in $files; do
    grep -q " $file\$" "$tmp/SHA256SUMS" || {
        echo "SHA256SUMS does not list $file; nothing was installed" >&2
        exit 1
    }
done

if [ -w "$dir" ]; then
    install -m 755 "$tmp/$bin" "$dir/cairo"
else
    echo "installing to $dir needs root"
    sudo install -m 755 "$tmp/$bin" "$dir/cairo"
fi
if [ -n "$libc" ]; then
    # The kit's paths are the ones its PHP was built with, under /opt/cairo.
    if [ -w / ]; then tar -C / -xzf "$tmp/$bin-dev.tar.gz"; else sudo tar -C / -xzf "$tmp/$bin-dev.tar.gz"; fi
fi
"$dir/cairo" version
