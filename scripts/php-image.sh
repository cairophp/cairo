#!/usr/bin/env bash
# Prints the official php image that Dockerfile.native builds on for a PHP
# version (8.4 or 8.4.28): thread-safe, with the embed SAPI. Before 8.6 that
# is the zts variant; from 8.6 on the default images are thread-safe.
set -euo pipefail

minor="$(echo "${1:?usage: scripts/php-image.sh 8.4}" | cut -d. -f1,2)"
case "$minor" in
  8.[3-5]) echo "php:$minor-zts-trixie" ;;
  8.[6-9] | 9.*) echo "php:$minor-cli-trixie" ;;
  *) echo "PHP $1: native images need PHP 8.3 or later" >&2; exit 1 ;;
esac
