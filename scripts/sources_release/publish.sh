#!/usr/bin/env bash
set -euo pipefail
: "${SOURCE_SHA:?}" "${VERSION:?}"
exec python3 scripts/sources_release/publish.py "${1:?verified bundle required}" \
  --sha "$SOURCE_SHA" --version "$VERSION" "${@:2}"
