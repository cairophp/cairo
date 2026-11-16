#!/usr/bin/env bash
# Builds Cairo as one static binary: a thread-safe (ZTS) PHP with the embed
# SAPI as a static library, its extensions and their C libraries all linked
# into the Go binary. Nothing else is needed at run time, not even libc.
#
# Meant for Alpine (musl): see Dockerfile.static, which installs the static
# libraries and runs this script. On glibc the binary links but glibc's
# static getaddrinfo still loads NSS modules at run time, so prefer musl.
#
# Usage: scripts/build-static.sh [php-version] [output]
#   PHP_SRC, CURL_SRC   unpacked sources to use instead of downloading
#   PHP_EXTENSIONS      configure flags for the extensions (default below)
#   WORK                build directory (default: a temporary one)
set -euo pipefail

PHP_VERSION="${1:-8.3.27}"
OUT="${2:-dist/cairo}"
CURL_VERSION="${CURL_VERSION:-8.11.1}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
case "$OUT" in /*) ;; *) OUT="$ROOT/$OUT" ;; esac
WORK="${WORK:-$(mktemp -d)}"
DEPS="$WORK/deps"
PREFIX="$WORK/php"
JOBS="$(nproc)"
mkdir -p "$DEPS" "$(dirname "$OUT")"

# Every library is linked statically, so pkg-config must list what each
# library needs in turn.
export PKG_CONFIG="pkg-config --static"
export PKG_CONFIG_PATH="$DEPS/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"

# libcurl, built small: TLS through OpenSSL and zlib, nothing else (no LDAP,
# Kerberos, SSH or IDN), so the link does not drag in libraries most distros
# ship only as shared objects. Cairo's curl hook and ext/curl both use it.
if [ -z "${CURL_SRC:-}" ]; then
  curl -fsSL "https://curl.se/download/curl-${CURL_VERSION}.tar.xz" | tar -xJ -C "$WORK"
  CURL_SRC="$WORK/curl-${CURL_VERSION}"
fi
(
  cd "$CURL_SRC"
  ./configure --prefix="$DEPS" --disable-shared --enable-static --with-pic \
    --with-openssl --with-zlib \
    --without-libpsl --without-libidn2 --without-nghttp2 --without-nghttp3 --without-ngtcp2 \
    --without-brotli --without-zstd --without-libssh2 --without-libssh --without-librtmp \
    --without-gssapi --disable-ldap --disable-ldaps --disable-manual --disable-docs
  make -j"$JOBS"
  make install
)

if [ -z "${PHP_SRC:-}" ]; then
  curl -fsSL "https://github.com/php/php-src/archive/refs/tags/php-${PHP_VERSION}.tar.gz" | tar -xz -C "$WORK"
  PHP_SRC="$WORK/php-src-php-${PHP_VERSION}"
fi

# The extensions of scripts/build-php.sh. Opcache is left out: before PHP
# 8.5 it can only be built as a shared extension (opcache.so), which a static
# binary cannot load; PHP 8.5 and later build it in on their own.
DEFAULT_EXTENSIONS="
  --enable-pdo --enable-mysqlnd --with-pdo-mysql=mysqlnd --with-mysqli=mysqlnd
  --with-pdo-sqlite --with-sqlite3
  --with-curl --with-openssl
  --enable-mbstring --enable-intl --with-sodium
  --with-zip --with-zlib --with-bz2 --with-gmp
  --enable-bcmath --enable-calendar --enable-exif --enable-sockets
  --enable-gd --with-jpeg --with-webp --with-freetype
  --with-iconv
  --without-readline --without-pgsql --without-pdo-pgsql"
(
  cd "$PHP_SRC"
  [ -x configure ] || ./buildconf --force
  # shellcheck disable=SC2086
  ./configure \
    --prefix="$PREFIX" \
    --enable-zts \
    --enable-embed=static \
    --disable-shared \
    --disable-cli --disable-cgi --disable-phpdbg \
    ${PHP_EXTENSIONS:-$DEFAULT_EXTENSIONS}
  make -j"$JOBS"
  make install
)

PHP_CONFIG="$PREFIX/bin/php-config"
export CGO_ENABLED=1
export CGO_CFLAGS="$($PHP_CONFIG --includes) -I$DEPS/include -DCAIRO_STATIC_CURL"
# libphp.a first, then what it needs. The group lets the linker go back and
# forth between libraries that need each other. ext/intl is C++.
# Cairo's curl hook takes over two libcurl functions by renaming calls to
# them (see internal/sapi/curl_hook.c).
export CGO_LDFLAGS="-Wl,--wrap=curl_easy_perform -Wl,--wrap=curl_multi_wait -L$PREFIX/lib -L$DEPS/lib -Wl,--start-group -lphp $($PHP_CONFIG --libs) $(pkg-config --libs libcurl) -Wl,--end-group -lstdc++ -lm"

cd "$ROOT"
go build -trimpath -tags netgo,osusergo \
  -ldflags "-s -w -linkmode external -extldflags '-static'" \
  -o "$OUT" ./cmd/cairo
echo "static cairo with PHP $PHP_VERSION: $OUT"
