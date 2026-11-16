PHP_PREFIX ?= /opt/php-zts
PHP_CONFIG ?= $(PHP_PREFIX)/bin/php-config
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

export CGO_ENABLED = 1
export CGO_CFLAGS  = $(shell $(PHP_CONFIG) --includes)
export CGO_LDFLAGS = -L$(PHP_PREFIX)/lib -Wl,-rpath,$(PHP_PREFIX)/lib

.PHONY: build test run php static stubs

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/cairo ./cmd/cairo

test:
	go vet ./...
	go test ./... -count=1 -v

run: build
	./bin/cairo serve examples/greeter/server.php

# Build PHP (ZTS + embed, opcache and the extensions in scripts/build-php.sh) into $(PHP_PREFIX).
# Needs a C toolchain, bison, re2c and the -dev packages listed in that script.
php:
	scripts/build-php.sh 8.3.27 $(PHP_PREFIX)

# One static binary (musl, PHP and its extensions linked in) in dist/cairo. Needs Docker.
static:
	docker build -f Dockerfile.static --build-arg CAIRO_VERSION=$(VERSION) --output dist .

# Regenerate stubs/cairo.php (Cairo's PHP API for IDEs) from php/runtime.php.
stubs:
	php scripts/stubs.php php/runtime.php > stubs/cairo.php
