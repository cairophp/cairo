#!/usr/bin/env bash
# Publishes stubs/ as the cairophp/stubs Composer package: replaces the
# contents of a clone of that repository with stubs/ and LICENSE, commits,
# and tags the commit like the Cairo release it came from.
#
#   scripts/publish-stubs.sh path/to/stubs-clone v0.1.1
set -euo pipefail

clone=$1
tag=$2
repo=$(cd "$(dirname "$0")/.." && pwd)

git -C "$clone" rm -rq --ignore-unmatch .
cp "$repo"/stubs/* "$repo/LICENSE" "$clone/"
git -C "$clone" add -A
git -C "$clone" commit -q --allow-empty -m "Cairo $tag"
git -C "$clone" tag "$tag"
git -C "$clone" push -q origin HEAD:main "$tag"
