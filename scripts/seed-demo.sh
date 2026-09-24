#!/bin/sh
# Task passes the Compose executable and arguments without evaluating a string.
set -eu
umask 077
device=${THERMORA_DEVICE:-}
case "$device" in
"")
  directory=.local
  set -- "$@" exec -T cloud thermora seed
  ;;
[!a-z]* | *[!a-z0-9-]*)
  printf '%s\n' 'Invalid device name; use lowercase letters, digits and hyphens, starting with a letter.' >&2
  exit 1
  ;;
*)
  if [ "${#device}" -gt 32 ]; then
    printf '%s\n' 'Device names must be at most 32 characters.' >&2
    exit 1
  fi
  directory=.local/devices/$device
  set -- "$@" exec -T cloud thermora seed "$device"
  ;;
esac
scratch=$(mktemp "$directory/demo-access.XXXXXX")
trap 'rm -f "$scratch"' EXIT HUP INT TERM
"$@" >"$scratch"
mv "$scratch" "$directory/demo-access.json"
printf 'Open https://localhost:8443; private access and claim_status are in %s/demo-access.json.\n' "$directory"
