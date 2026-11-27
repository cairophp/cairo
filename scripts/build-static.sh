#!/usr/bin/env bash
# Builds Cairo as one static binary: a thread-safe (ZTS) PHP with the embed
# SAPI as a static library, its extensions and their C libraries all linked
# into the Go binary. Nothing else is needed at run time, not even libc.
#
# On Alpine (musl, Dockerfile.static) the binary is fully static and cannot
# load an extension: a static musl program has no dlopen(). On glibc
# (Dockerfile.gnu) everything but glibc itself is linked in and PHP's symbols
# are exported, so it loads extensions built with phpize against the same PHP:
# the build also writes that PHP's headers, php-config and phpize next to the
# binary (<output>-dev.tar.gz, PHP installed under /opt/cairo/phpA.B).
#
# Usage: scripts/build-static.sh [php-version] [output]
#   PHP_SRC, CURL_SRC, LIBZIP_SRC, ICU_SRC, ONIG_SRC   unpacked sources to use instead of downloading
#   PHP_EXTENSIONS      configure flags for the extensions (default below)
#   EXTRA_EXTENSIONS    configure flags added to those, for PHP's own extensions
#                       left out by default and for the PECL ones below
#                       (--enable-apcu --enable-redis)
#   PECL_EXTENSIONS     PECL packages built in, as name-version (apcu-5.1.28
#                       redis-6.2.0): each is fetched from pecl.php.net into
#                       PHP's ext/ and needs its flag in EXTRA_EXTENSIONS. The C
#                       libraries one needs must be there as static libraries
#                       (Dockerfile.static: APK_PACKAGES)
#   PHP_PREFIX          where PHP is installed (default: $WORK/php, and
#                       /opt/cairo/phpA.B on glibc, where the kit unpacks)
#   WORK                build directory (default: a temporary one). Reusing
#                       one skips the libraries and PHP already built there.
#   STAGE=php           stop after PHP is built and its sources are removed
#                       (Dockerfile.static caches that as its own layer)
#   CAIRO_VERSION       what `cairo version` prints (default: dev)
#   CAIRO_TAGS          Go build tags (default: deprecated_topic,deprecated_claim,
#                       which the Mercure hub needs for clients of the protocol
#                       before 1.0; nomercure leaves the hub out)
set -euo pipefail

PHP_VERSION="${1:-8.6.0}"
OUT="${2:-dist/cairo}"
CURL_VERSION="${CURL_VERSION:-8.11.1}"
LIBZIP_VERSION="${LIBZIP_VERSION:-1.11.2}"
ICU_VERSION="${ICU_VERSION:-74.2}"
ONIG_VERSION="${ONIG_VERSION:-6.9.10}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
case "$OUT" in /*) ;; *) OUT="$ROOT/$OUT" ;; esac
WORK="${WORK:-$(mktemp -d)}"
DEPS="$WORK/deps"
PHP_MINOR="$(echo "$PHP_VERSION" | cut -d. -f1,2)"
GLIBC=""
if getconf GNU_LIBC_VERSION >/dev/null 2>&1; then GLIBC=1; fi
if [ -n "$GLIBC" ]; then
  PREFIX="${PHP_PREFIX:-/opt/cairo/php$PHP_MINOR}"
else
  PREFIX="${PHP_PREFIX:-$WORK/php}"
fi
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

# Oniguruma for mbstring. Debian ships it only as a shared library.
if [ -n "$GLIBC" ] && ! built onig; then
if [ -z "${ONIG_SRC:-}" ]; then
  curl -fsSL "https://github.com/kkos/oniguruma/releases/download/v${ONIG_VERSION}/onig-${ONIG_VERSION}.tar.gz" | tar -xz -C "$WORK"
  ONIG_SRC="$WORK/onig-${ONIG_VERSION}"
fi
cmake -S "$ONIG_SRC" -B "$WORK/onig-build" -DCMAKE_INSTALL_PREFIX="$DEPS" -DCMAKE_INSTALL_LIBDIR=lib \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DBUILD_SHARED_LIBS=OFF -DBUILD_TEST=OFF
cmake --build "$WORK/onig-build" -j"$JOBS"
cmake --install "$WORK/onig-build"
mark onig
fi

# ICU for ext/intl, with its locale data inside libicudata.a. Alpine ships
# the data as a separate .dat file, which a static binary would not find
# (NumberFormatter and friends then fail with "Constructor failed"). Debian's
# libicudata.a has its data, and its libxml2.a needs that ICU.
if [ -z "$GLIBC" ] && ! built icu; then
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

# Opcache. PHP 8.5 and later build it in on their own. Before 8.5 it can only
# be built as a shared extension (opcache.so), which a static binary cannot
# load, so it is patched to be built in, as static-php-cli does (the patches
# are its own: see third_party/NOTICES). Its JIT is left out there: with ZTS
# on musl it crashes on x86-64 before 8.5.
# The CLI is not needed, as the binary is PHP's command line itself, but from
# 8.6 the embed SAPI uses the CLI's sources and configure refuses
# --disable-cli with --enable-embed.
OPCACHE=""
NO_CLI="--disable-cli"
case "$PHP_MINOR" in
  8.3 | 8.4)
    if [ ! -e "$PHP_SRC/.cairo-static-opcache" ]; then
      patch -d "$PHP_SRC" -p1 < "$ROOT/scripts/php-patches/static-opcache-$PHP_MINOR.patch"
      touch "$PHP_SRC/.cairo-static-opcache"
      rm -f "$PHP_SRC/configure"   # it comes from the patched config.m4
    fi
    OPCACHE="--enable-opcache --disable-opcache-jit" ;;
  8.5) ;;
  8.[6-9] | 9.*) NO_CLI="" ;;
  *) echo "PHP $PHP_VERSION: static builds need PHP 8.3 or later" >&2; exit 1 ;;
esac

# The extensions of scripts/build-php.sh.
DEFAULT_EXTENSIONS="
  --enable-pdo --enable-mysqlnd --with-pdo-mysql=mysqlnd --with-mysqli=mysqlnd
  --with-pdo-sqlite --with-sqlite3
  --with-curl --with-openssl
  --enable-mbstring --enable-intl --with-sodium
  --with-zip --with-zlib --with-bz2 --with-gmp
  --enable-bcmath --enable-calendar --enable-exif --enable-sockets --enable-pcntl
  --enable-gd --with-jpeg --with-webp --with-freetype
  --with-iconv
  --without-readline --without-pgsql --without-pdo-pgsql"
for pkg in ${PECL_EXTENSIONS:-}; do
  name="${pkg%-*}"
  version="${pkg##*-}"
  case "$name:$version" in
    *[!a-z0-9_:.A-Za-z]* | :* | *: | "$pkg:$pkg") echo "PECL_EXTENSIONS: $pkg is not name-version" >&2; exit 1 ;;
  esac
  if [ ! -d "$PHP_SRC/ext/$name" ]; then
    curl -fsSL "https://pecl.php.net/get/${pkg}.tgz" | tar -xz -C "$WORK"
    mv "$WORK/$pkg" "$PHP_SRC/ext/$name"
    rm -f "$PHP_SRC/configure"   # buildconf finds the new extension's config.m4
  fi
done
(
  cd "$PHP_SRC"
  [ -x configure ] || ./buildconf --force
  # shellcheck disable=SC2086
  ./configure \
    --prefix="$PREFIX" \
    --enable-zts \
    --enable-embed=static \
    --disable-shared \
    $NO_CLI --disable-cgi --disable-phpdbg \
    $OPCACHE ${PHP_EXTENSIONS:-$DEFAULT_EXTENSIONS} ${EXTRA_EXTENSIONS:-}
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
# shellcheck disable=SC2046
LIBS="-lphp $($PHP_CONFIG --libs) $(pkg-config --libs libcurl libzip)"
WRAP="-Wl,--wrap=curl_easy_perform -Wl,--wrap=curl_multi_wait -Wl,--wrap=curl_easy_cleanup -L$PREFIX/lib -L$DEPS/lib"
if [ -z "$GLIBC" ]; then
  export CGO_LDFLAGS="$WRAP -Wl,--start-group $LIBS -Wl,--end-group -lstdc++ -lm"
  EXTLDFLAGS="-static"
else
  # Every library is linked in but glibc's own, which stay shared: a program
  # that links glibc statically cannot load an extension either. The
  # distro's .so files sit next to its .a files, so -Bstatic picks the
  # archives. --export-dynamic lets an extension find PHP's functions.
  static="" shared=""
  for flag in $LIBS; do
    case "$flag" in
      -lc | -lm | -ldl | -lrt | -lpthread | -lresolv | -lutil | -lcrypt | -pthread) shared="$shared $flag" ;;
      *) static="$static $flag" ;;
    esac
  done
  export CGO_LDFLAGS="$WRAP -Wl,-Bstatic -Wl,--start-group $static -l:libstdc++.a -Wl,--end-group -Wl,-Bdynamic $shared -lm"
  EXTLDFLAGS="-Wl,--export-dynamic -static-libgcc"
fi

cd "$ROOT"
go build -trimpath -tags "netgo,osusergo,${CAIRO_TAGS:-deprecated_topic,deprecated_claim}" \
  -ldflags "-s -w -X main.version=${CAIRO_VERSION:-dev} -linkmode external -extldflags '$EXTLDFLAGS'" \
  -o "$OUT" ./cmd/cairo
echo "cairo ${CAIRO_VERSION:-dev} with PHP $PHP_VERSION: $OUT"

if [ -n "$GLIBC" ]; then
  # What phpize needs to build an extension for this PHP, at the paths it
  # was built with: unpacked in /, `$PREFIX/bin/phpize` works there.
  EXT_DIR="$($PHP_CONFIG --extension-dir)"
  mkdir -p "$EXT_DIR"
  tar -czf "$OUT-dev.tar.gz" -C / "${PREFIX#/}/bin/php-config" "${PREFIX#/}/bin/phpize" \
    "${PREFIX#/}/include/php" "${PREFIX#/}/lib/php/build" "${EXT_DIR#/}"
  echo "the headers and phpize of PHP $PHP_VERSION for extensions: $OUT-dev.tar.gz"
fi
