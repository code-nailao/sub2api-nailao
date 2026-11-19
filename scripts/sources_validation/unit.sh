#!/usr/bin/env bash
# Backend unit tests.
#
#   unit.sh            run everything in one process tree (local default)
#   unit.sh other      every package except internal/service
#   unit.sh service-N  one of four slices of internal/service (N = 1..4)
#
# CI runs the five slices as parallel jobs. internal/service holds about two
# thirds of the unit-test time in a single package, and Go runs the tests of one
# package one after another, so more cores do not help until the package is
# split. Most of that time is a hundred or so tests that wait on timeouts and
# retries.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT/backend"

SERVICE=./internal/service
# The unchanged upstream test measures the ENTIRE process heap. Other service
# tests leave background activity, which contaminates that measurement. Run it
# in its own process with the original 8 MiB bound; no test is omitted/relaxed.
MEMORY_TEST=TestInflightEstimate_AccountMappingNoDBAndBoundedMemory

# Slices are chosen by the first letter after "Test". The first three are
# positive prefixes; the fourth is "whatever those three do not match". Together
# they select every test exactly once, and a test added later cannot fall
# between them. Keep SLICED equal to the union of the three prefixes.
SLICE_1='^Test[A-F]'
SLICE_2='^Test[G-N]'
SLICE_3='^TestO'
SLICED='^Test[A-O]'

run() {
  go test -mod=readonly -p "${GO_TEST_P:-2}" -tags=unit "$@"
}

memory_test() {
  go test -mod=readonly -tags=unit -list "^${MEMORY_TEST}$" "$SERVICE" | grep -Fx "$MEMORY_TEST"
  go test -mod=readonly -tags=unit -run "^${MEMORY_TEST}$" "$SERVICE"
}

case "${1:-all}" in
  all)
    run -skip "^${MEMORY_TEST}$" ./...
    memory_test
    ;;
  other)
    # shellcheck disable=SC2046 # package paths contain no spaces
    run $(go list -tags=unit ./... | grep -v '/internal/service$')
    ;;
  service-1)
    run -run "$SLICE_1" "$SERVICE"
    ;;
  service-2)
    # The memory test's name falls in this slice; it still runs alone.
    run -run "$SLICE_2" -skip "^${MEMORY_TEST}$" "$SERVICE"
    memory_test
    ;;
  service-3)
    run -run "$SLICE_3" "$SERVICE"
    ;;
  service-4)
    run -skip "$SLICED" "$SERVICE"
    ;;
  *)
    echo "usage: unit.sh [all|other|service-1|service-2|service-3|service-4]" >&2
    exit 2
    ;;
esac
