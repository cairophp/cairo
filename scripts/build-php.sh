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

# The extensions most PHP apps and frameworks expect (Laravel, Symfony,
# WordPress), plus opcache, which Cairo loads by default. Left out: ext/pgsql
# and pdo_pgsql (Cairo has its own Go-backed pdo pgsql driver), pcntl (forking
# a threaded server), readline. System packages (Debian/Ubuntu):
#   build-essential autoconf bison re2c pkg-config libxml2-dev libsqlite3-dev
#   libcurl4-openssl-dev libssl-dev libonig-dev libicu-dev libsodium-dev
#   libzip-dev zlib1g-dev libbz2-dev libgmp-dev libpng-dev libjpeg-dev
#   libwebp-dev libfreetype-dev
./configure \
  --prefix="$PREFIX" \
  --enable-zts \
  --enable-embed=shared \
  --disable-cgi \
  --disable-phpdbg \
  --enable-cli \
  --enable-opcache \
  --enable-pdo \
  --enable-mysqlnd \
  --with-pdo-mysql=mysqlnd \
  --with-mysqli=mysqlnd \
  --with-pdo-sqlite \
  --with-sqlite3 \
  --with-curl \
  --with-openssl \
  --enable-mbstring \
  --enable-intl \
  --with-sodium \
  --with-zip \
  --with-zlib \
  --with-bz2 \
  --with-gmp \
  --enable-bcmath \
  --enable-calendar \
  --enable-exif \
  --enable-sockets \
  --enable-gd \
  --with-jpeg \
  --with-webp \
  --with-freetype \
  --with-iconv \
  --without-readline \
  --without-pgsql \
  --without-pdo-pgsql
make -j"$(nproc)"
make install
echo "PHP $VERSION (ZTS, embed) installed in $PREFIX"
