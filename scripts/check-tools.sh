#!/bin/sh
set -eu

check() {
  expected=$(mise exec -- "$@")
  actual=$("$@" 2>&1) || actual=unavailable
  if [ "$actual" != "$expected" ]; then
    printf '%s\n' "Toolchain mismatch: $*" "Expected: $expected" "Active: $actual" >&2
    printf '%s\n' 'Enter a clean project shell: mise en -s "bash --noprofile --norc"' >&2
    exit 1
  fi
}

check go version
check go tool compile -V
check node --version
check npm --version
check aws --version
check task --version
check golangci-lint version
