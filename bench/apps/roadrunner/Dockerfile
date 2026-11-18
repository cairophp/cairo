FROM ghcr.io/roadrunner-server/roadrunner:2025.1.15 AS rr

FROM php:8.5.12-cli AS gen
RUN apt-get update && apt-get install -y --no-install-recommends curl unzip ca-certificates \
    && curl -fsSL -o /tmp/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v33.1/protoc-33.1-linux-x86_64.zip \
    && unzip -q /tmp/protoc.zip -d /opt/protoc
COPY roadrunner/greeter.proto /src/greeter.proto
RUN mkdir /gen && /opt/protoc/bin/protoc -I /src --php_out=/gen /src/greeter.proto

FROM php:8.5.12-cli
COPY --from=mlocati/php-extension-installer:latest /usr/bin/install-php-extensions /usr/local/bin/
RUN install-php-extensions sockets pdo_pgsql pdo_mysql protobuf @composer
COPY --from=rr /usr/bin/rr /usr/local/bin/rr
COPY php.ini /usr/local/etc/php/conf.d/zz-bench.ini
COPY . /app
WORKDIR /app/roadrunner
COPY --from=gen /gen ./gen
RUN cp GreeterInterface.php gen/Rr/Helloworld/ && composer install --no-dev --classmap-authoritative --no-interaction --quiet
CMD ["rr", "serve", "-c", "/app/roadrunner/.rr.yaml"]
