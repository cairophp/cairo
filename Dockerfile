# Builds Cairo against a ZTS PHP with the embed SAPI, then ships a slim image.
FROM golang:1.27-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential autoconf bison re2c pkg-config xz-utils curl ca-certificates \
      libxml2-dev libsqlite3-dev libcurl4-openssl-dev libssl-dev libonig-dev libicu-dev libsodium-dev libzip-dev zlib1g-dev libbz2-dev libgmp-dev libpng-dev libjpeg-dev libwebp-dev libfreetype-dev \
    && rm -rf /var/lib/apt/lists/*
COPY scripts/build-php.sh /tmp/build-php.sh
RUN /tmp/build-php.sh 8.6.0 /opt/php-zts
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
      libxml2 libsqlite3-0 libcurl4 libssl3 libonig5 libicu72 libsodium23 libzip4 zlib1g libbz2-1.0 libgmp10 \
      libpng16-16 libjpeg62-turbo libwebp7 libfreetype6 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /opt/php-zts/lib/libphp.so /opt/php-zts/lib/libphp.so
# Shared extensions (opcache.so) at the path PHP was built with.
COPY --from=build /opt/php-zts/lib/php/extensions /opt/php-zts/lib/php/extensions
COPY --from=build /src/bin/cairo /usr/local/bin/cairo
COPY examples /app/examples
WORKDIR /app
EXPOSE 8080 9000
ENTRYPOINT ["cairo", "serve"]
CMD ["examples/greeter/server.php"]
