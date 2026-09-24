#!/bin/sh
# Quiesced local-cloud snapshot. Never selects a global Kubernetes context.
set -eu
umask 077
node=floci-eks-thermora-cloud
image=postgres:18.6-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873
counts="SELECT json_build_object('devices',(SELECT count(*) FROM devices),'telemetry',(SELECT count(*) FROM telemetry),'commands',(SELECT count(*) FROM commands),'events',(SELECT count(*) FROM command_events),'runs',(SELECT count(*) FROM simulation_runs),'audit',(SELECT count(*) FROM audit),'comparisons',(SELECT count(*) FROM comparisons),'replays',(SELECT count(*) FROM observed_replays));"
checksum() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}
case "${1:-}" in
backup)
  docker image inspect "$image" >/dev/null
  for app in cloud mqtt; do
    replicas=$(docker exec "$node" kubectl -n thermora get deployment "$app" -o jsonpath='{.spec.replicas}')
    [ "$replicas" = 1 ] || {
      echo 'Start the local cloud profile before backing it up.' >&2
      exit 1
    }
  done
  mkdir -p .local/backups
  directory=$(mktemp -d .local/backups/cloud-XXXXXXXX)
  services=$(sh scripts/cloud-compose.sh config --services | sed -n '/^edge\(-.*\)\{0,1\}$/p; /^sim\(-.*\)\{0,1\}$/p')
  [ -n "$services" ] || exit 1
  resume() {
    result=$?
    trap - EXIT HUP INT TERM
    sh scripts/cloud-compose.sh start floci >/dev/null || exit 1
    sh scripts/cloud.sh start || exit 1
    docker exec "$node" kubectl -n thermora scale deployment/mqtt deployment/cloud --replicas=1 >/dev/null || exit 1
    # shellcheck disable=SC2086 # Validated Compose service names, intentionally separate arguments.
    sh scripts/cloud-compose.sh start $services >/dev/null || exit 1
    docker exec "$node" kubectl -n thermora rollout status deployment/cloud --timeout=120s || exit 1
    exit "$result"
  }
  trap resume EXIT HUP INT TERM
  # shellcheck disable=SC2086 # Names come only from Compose's validated service list.
  sh scripts/cloud-compose.sh stop $services
  docker exec "$node" kubectl -n thermora scale deployment/cloud deployment/mqtt --replicas=0
  docker exec "$node" kubectl -n thermora wait --for=delete pod -l 'app in (cloud,mqtt)' --timeout=60s
  docker exec "$node" kubectl -n thermora exec deployment/postgres -- pg_dump -U thermora -d thermora -Fc >"$directory/postgres.dump"
  docker exec "$node" kubectl -n thermora exec deployment/postgres -- psql -U thermora -d thermora -Atc "$counts" >"$directory/counts.json"
  for claim in mqtt manufacturer; do
    volume=$(docker exec "$node" kubectl -n thermora get pvc "$claim" -o jsonpath='{.spec.volumeName}')
    path=$(docker exec "$node" kubectl get pv "$volume" -o jsonpath='{.spec.hostPath.path}')
    if [ -z "$path" ]; then
      path=$(docker exec "$node" kubectl get pv "$volume" -o jsonpath='{.spec.local.path}')
    fi
    case "$path" in /var/lib/rancher/k3s/storage/pvc-*) ;; *)
      echo 'Unexpected local volume path.' >&2
      exit 1
      ;;
    esac
    docker exec "$node" tar -C "$path" -czf - . >"$directory/$claim.tar.gz"
  done
  sh scripts/cloud-compose.sh stop floci
  # Floci can return while its managed node is still stopping. Join that shutdown
  # before resume calls docker start, which otherwise sees an already-running node.
  sh scripts/cloud.sh stop
  absolute=$(CDPATH='' cd "$directory" && pwd)
  docker run --rm --pull=never --network none -v thermora-cloud_floci-data:/source:ro -v "$absolute:/backup" "$image" sh -c 'tar -C /source -czf /backup/floci.tar.gz .; chmod 600 /backup/floci.tar.gz; chown "$1:$2" /backup/floci.tar.gz' backup "$(id -u)" "$(id -g)"
  tar -C .local -czf "$directory/local-cloud.tar.gz" cloud
  git rev-parse HEAD >"$directory/revision.txt"
  git diff --stat >>"$directory/revision.txt"
  (cd "$directory" && checksum postgres.dump counts.json mqtt.tar.gz manufacturer.tar.gz floci.tar.gz local-cloud.tar.gz revision.txt >SHA256SUMS)
  printf '%s\n' "$directory" >.local/backups/latest
  printf 'Private backup complete: %s\nVerify with: task restore FILE=%s\n' "$directory" "$directory"
  ;;
verify)
  directory=${2:-}
  [ -n "$directory" ] && [ -f "$directory/SHA256SUMS" ] || {
    echo 'Choose a backup directory with FILE=.' >&2
    exit 1
  }
  directory=$(CDPATH='' cd "$directory" && pwd)
  (cd "$directory" && checksum -c SHA256SUMS)
  scratch=$(mktemp -d)
  container=thermora-restore-$(basename "$scratch" | tr '[:upper:]' '[:lower:]')
  cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
    rm -rf "$scratch"
  }
  trap cleanup EXIT HUP INT TERM
  docker run --pull=never -d --name "$container" --network none --tmpfs /var/lib/postgresql --tmpfs /tmp -e POSTGRES_HOST_AUTH_METHOD=trust "$image" >/dev/null
  ready=0
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if docker exec "$container" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  [ "$ready" = 1 ] || {
    echo 'Isolated restore database failed to start.' >&2
    exit 1
  }
  docker exec -i "$container" pg_restore -U postgres -d postgres --no-owner --no-acl --exit-on-error <"$directory/postgres.dump"
  docker exec "$container" psql -U postgres -d postgres -Atc "$counts" >"$scratch/counts.json"
  cmp "$directory/counts.json" "$scratch/counts.json"
  for archive in mqtt manufacturer floci local-cloud; do tar -tzf "$directory/$archive.tar.gz" >/dev/null; done
  echo 'Isolated PostgreSQL restore, recorded counts, archive readability and hashes verified. Live control state was not replaced.'
  ;;
*)
  echo 'Usage: cloud-backup.sh backup | verify DIRECTORY' >&2
  exit 2
  ;;
esac
