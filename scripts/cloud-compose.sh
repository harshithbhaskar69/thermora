#!/bin/sh
set -eu
if [ -f .local/cloud/devices/compose.json ]; then
  set -- -f .local/cloud/devices/compose.json "$@"
fi
set -- -f .local/cloud/gateway.json "$@"
exec docker compose --env-file .local/cloud/compose.env -f deploy/cloud/compose.yaml "$@"
