# Builds Cairo against a ZTS PHP with the embed SAPI, then ships a slim image.
FROM golang:1.24-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential autoconf bison re2c pkg-config libxml2-dev libsqlite3-dev libcurl4-openssl-dev xz-utils curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY scripts/build-php.sh /tmp/build-php.sh
RUN /tmp/build-php.sh 8.3.27 /opt/php-zts
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends libxml2 libsqlite3-0 libcurl4 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /opt/php-zts/lib/libphp.so /opt/php-zts/lib/libphp.so
COPY --from=build /src/bin/cairo /usr/local/bin/cairo
COPY examples /app/examples
WORKDIR /app
EXPOSE 8080 9000
ENTRYPOINT ["cairo", "serve"]
CMD ["examples/greeter/server.php"]
