# The static binary for glibc, which loads extensions: Cairo, PHP and their C
# libraries linked in, glibc itself shared, PHP's symbols exported:
#   docker build -f Dockerfile.gnu --output dist .
# writes dist/cairo and dist/cairo-dev.tar.gz, the headers, php-config and
# phpize of its PHP for building extensions (see scripts/build-static.sh).
# Built on Debian 12, so it runs where glibc is 2.36 or newer.
FROM golang:1.27-trixie AS go

FROM debian:bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential autoconf bison flex re2c pkg-config cmake curl ca-certificates xz-utils perl patch file \
      libssl-dev zlib1g-dev libbz2-dev liblzma-dev libxml2-dev libsqlite3-dev libicu-dev \
      libsodium-dev libgmp-dev libpng-dev libjpeg-dev libwebp-dev libfreetype-dev libbrotli-dev \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
WORKDIR /src
# The libraries and PHP, cached apart from Cairo's code as in Dockerfile.static.
ARG PHP_VERSION=8.6.0
ENV WORK=/work
COPY scripts/build-static.sh scripts/
COPY scripts/php-patches scripts/php-patches
RUN STAGE=php scripts/build-static.sh "$PHP_VERSION"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CAIRO_VERSION=dev
ARG CAIRO_TAGS=deprecated_topic,deprecated_claim
RUN --mount=type=cache,target=/root/.cache/go-build scripts/build-static.sh "$PHP_VERSION" /out/cairo

FROM scratch
COPY --from=build /out/cairo /out/cairo-dev.tar.gz /
