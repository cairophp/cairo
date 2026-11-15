#!/usr/bin/env bash
# Builds a thread-safe (ZTS) PHP with the embed SAPI (libphp.so), which Cairo
# links against. Usage: scripts/build-php.sh [version] [prefix]
set -euo pipefail

VERSION="${1:-8.3.27}"
PREFIX="${2:-/opt/php-zts}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

curl -fsSL "https://github.com/php/php-src/archive/refs/tags/php-${VERSION}.tar.gz" -o "$WORK/php.tar.gz"
tar -xzf "$WORK/php.tar.gz" -C "$WORK"
cd "$WORK/php-src-php-${VERSION}"
./buildconf --force

# A small build for now: Core, date, hash, json, pcre, random, Reflection, SPL, standard.
# Add extensions here as Cairo needs them (opcache, mbstring, pdo, ...).
./configure \
  --prefix="$PREFIX" \
  --enable-zts \
  --enable-embed=shared \
  --disable-all \
  --disable-cgi \
  --disable-phpdbg \
  --enable-cli
make -j"$(nproc)"
make install
echo "PHP $VERSION (ZTS, embed) installed in $PREFIX"
