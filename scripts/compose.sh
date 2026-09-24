#!/bin/sh
# Keep all named device pairs in the same up/down lifecycle as the base system.
set -eu
if [ -f .local/manufacturer/config.json ]; then
  set -- -f deploy/compose/onboarding.yaml "$@"
fi
if [ -f .local/devices/compose.json ]; then
  set -- -f .local/devices/compose.json "$@"
fi
exec docker compose --env-file .local/compose.env -f deploy/compose/compose.yaml "$@"
