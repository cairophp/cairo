PHP_PREFIX ?= /opt/php-zts
# The newest PHP of .github/php-versions.json.
PHP_VERSION ?= 8.6.0
PHP_CONFIG ?= $(PHP_PREFIX)/bin/php-config
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The Mercure hub (AGPL-3.0) is linked in with what it needs to take clients
# of the protocol before 1.0. `make build TAGS=nomercure` leaves the hub out.
TAGS ?= deprecated_topic,deprecated_claim

export CGO_ENABLED = 1
# Go's build cache does not see PHP's headers change: the version in the
# flags keeps it from linking code compiled against another PHP.
export CGO_CFLAGS  = $(shell $(PHP_CONFIG) --includes) -DCAIRO_PHP_VERNUM=$(shell $(PHP_CONFIG) --vernum)
export CGO_LDFLAGS = -L$(PHP_PREFIX)/lib -Wl,-rpath,$(PHP_PREFIX)/lib

.PHONY: build test run php static stubs

build:
	go build -tags "$(TAGS)" -ldflags "-X main.version=$(VERSION)" -o bin/cairo ./cmd/cairo

test:
	go vet -tags "$(TAGS)" ./...
	go vet -tags nomercure ./...
	go test -tags "$(TAGS)" ./... -count=1 -v
	go test -tags nomercure ./internal/mercurehub -count=1

run: build
	./bin/cairo serve examples/greeter/server.php

# Build PHP (ZTS + embed, opcache and the extensions in scripts/build-php.sh) into $(PHP_PREFIX).
# Needs a C toolchain, bison, re2c and the -dev packages listed in that script.
php:
	scripts/build-php.sh $(PHP_VERSION) $(PHP_PREFIX)

# One static binary (musl, PHP and its extensions linked in) in dist/cairo. Needs Docker.
static:
	docker build -f Dockerfile.static --build-arg CAIRO_VERSION=$(VERSION) --build-arg CAIRO_TAGS=$(TAGS) --output dist .

# Regenerate stubs/cairo.php (Cairo's PHP API for IDEs) from the PHP files of the core and the extensions.
stubs:
	php scripts/stubs.php > stubs/cairo.php
