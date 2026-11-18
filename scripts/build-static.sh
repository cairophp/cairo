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
#   PHP_SRC, CURL_SRC, LIBZIP_SRC, ICU_SRC   unpacked sources to use instead of downloading
#   PHP_EXTENSIONS      configure flags for the extensions (default below)
#   WORK                build directory (default: a temporary one). Reusing
#                       one skips the libraries and PHP already built there.
#   STAGE=php           stop after PHP is built and its sources are removed
#                       (Dockerfile.static caches that as its own layer)
#   CAIRO_VERSION       what `cairo version` prints (default: dev)
set -euo pipefail

PHP_VERSION="${1:-8.5.12}"
OUT="${2:-dist/cairo}"
CURL_VERSION="${CURL_VERSION:-8.11.1}"
LIBZIP_VERSION="${LIBZIP_VERSION:-1.11.2}"
ICU_VERSION="${ICU_VERSION:-74.2}"
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

# Each library and PHP itself is built once per WORK: a marker file records
# what is installed, so a later run only builds Cairo.
built() { [ -e "$WORK/.built-$1" ]; }
mark() { touch "$WORK/.built-$1"; }

# libcurl, built small: TLS through OpenSSL and zlib, nothing else (no LDAP,
# Kerberos, SSH or IDN), so the link does not drag in libraries most distros
# ship only as shared objects. Cairo's curl hook and ext/curl both use it.
if ! built curl; then
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
mark curl
fi

# libzip for ext/zip, built the same way: Alpine ships it only as a shared
# library. Deflate through zlib only (no bzip2, xz, zstd or encryption).
if ! built libzip; then
if [ -z "${LIBZIP_SRC:-}" ]; then
  curl -fsSL "https://github.com/nih-at/libzip/releases/download/v${LIBZIP_VERSION}/libzip-${LIBZIP_VERSION}.tar.xz" | tar -xJ -C "$WORK"
  LIBZIP_SRC="$WORK/libzip-${LIBZIP_VERSION}"
fi
cmake -S "$LIBZIP_SRC" -B "$WORK/libzip-build" -DCMAKE_INSTALL_PREFIX="$DEPS" -DCMAKE_INSTALL_LIBDIR=lib \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DBUILD_SHARED_LIBS=OFF \
  -DENABLE_BZIP2=OFF -DENABLE_LZMA=OFF -DENABLE_ZSTD=OFF \
  -DENABLE_OPENSSL=OFF -DENABLE_GNUTLS=OFF -DENABLE_MBEDTLS=OFF -DENABLE_COMMONCRYPTO=OFF -DENABLE_WINDOWS_CRYPTO=OFF \
  -DBUILD_TOOLS=OFF -DBUILD_REGRESS=OFF -DBUILD_OSSFUZZ=OFF -DBUILD_EXAMPLES=OFF -DBUILD_DOC=OFF
cmake --build "$WORK/libzip-build" -j"$JOBS"
cmake --install "$WORK/libzip-build"
mark libzip
fi

# ICU for ext/intl, with its locale data inside libicudata.a. Distros ship
# the data as a separate .dat file, which a static binary would not find
# (NumberFormatter and friends then fail with "Constructor failed").
if ! built icu; then
if [ -z "${ICU_SRC:-}" ]; then
  curl -fsSL "https://github.com/unicode-org/icu/releases/download/release-${ICU_VERSION//./-}/icu4c-${ICU_VERSION//./_}-src.tgz" | tar -xz -C "$WORK"
  ICU_SRC="$WORK/icu/source"
fi
(
  cd "$ICU_SRC"
  CFLAGS="-O2 -fPIC" CXXFLAGS="-O2 -fPIC" ./configure --prefix="$DEPS" \
    --enable-static --disable-shared --with-data-packaging=static \
    --disable-tests --disable-samples --disable-extras
  make -j"$JOBS"
  make install
)
mark icu
fi

if ! built "php-$PHP_VERSION"; then
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
mark "php-$PHP_VERSION"
fi

if [ "${STAGE:-}" = php ]; then
  # Keep only what the Go build links: the installed libraries and PHP.
  find "$WORK" -mindepth 1 -maxdepth 1 ! -name deps ! -name php ! -name '.built-*' -exec rm -rf {} +
  echo "libraries and PHP $PHP_VERSION built in $WORK"
  exit 0
fi

PHP_CONFIG="$PREFIX/bin/php-config"
export CGO_ENABLED=1
export CGO_CFLAGS="$($PHP_CONFIG --includes) -I$DEPS/include -DCAIRO_STATIC_CURL"
# libphp.a first, then what it needs. The group lets the linker go back and
# forth between libraries that need each other. ext/intl is C++.
# Cairo's curl hook takes over two libcurl functions by renaming calls to
# them (see internal/sapi/curl_hook.c).
export CGO_LDFLAGS="-Wl,--wrap=curl_easy_perform -Wl,--wrap=curl_multi_wait -Wl,--wrap=curl_easy_cleanup -L$PREFIX/lib -L$DEPS/lib -Wl,--start-group -lphp $($PHP_CONFIG --libs) $(pkg-config --libs libcurl libzip) -Wl,--end-group -lstdc++ -lm"

cd "$ROOT"
go build -trimpath -tags netgo,osusergo \
  -ldflags "-s -w -X main.version=${CAIRO_VERSION:-dev} -linkmode external -extldflags '-static'" \
  -o "$OUT" ./cmd/cairo
echo "static cairo ${CAIRO_VERSION:-dev} with PHP $PHP_VERSION: $OUT"
