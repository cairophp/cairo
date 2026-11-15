PHP_PREFIX ?= /opt/php-zts
PHP_CONFIG ?= $(PHP_PREFIX)/bin/php-config

export CGO_ENABLED = 1
export CGO_CFLAGS  = $(shell $(PHP_CONFIG) --includes)
export CGO_LDFLAGS = -L$(PHP_PREFIX)/lib -Wl,-rpath,$(PHP_PREFIX)/lib

.PHONY: build test run php

build:
	go build -o bin/cairo ./cmd/cairo

test:
	go vet ./...
	go test ./... -count=1 -v

run: build
	./bin/cairo serve examples/greeter/server.php

# Build PHP (ZTS + embed) into $(PHP_PREFIX). Needs a C toolchain, bison, re2c, libxml2-dev, libsqlite3-dev.
php:
	scripts/build-php.sh 8.3.27 $(PHP_PREFIX)
