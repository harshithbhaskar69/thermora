#!/bin/sh
# Deliberately scoped teardown: never prune a Docker daemon or change its context.
set -eu
set -f

project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"

cat <<'EOF'
Permanently remove Thermora from this machine:
  - Both Compose profiles, all simulated devices and the Floci Kubernetes node
  - Database, broker, manufacturer and Floci/S3 volumes
  - Thermora-built Docker images
  - Owned host entries and certificate trust (may request sudo)
  - .local/ INCLUDING credentials, history and backups, plus .task/
Source, installed tools/dependencies, shared base images and Docker build caches stay.
EOF
printf 'Delete all Thermora runtime data? [y/N] '
if ! IFS= read -r answer; then answer=n; fi
case "$answer" in
y | Y | yes | YES) ;;
*)
  echo 'Cancelled. Nothing was changed.'
  exit 0
  ;;
esac

# A remote Docker context must never receive this machine's cleanup commands.
if [ -n "${DOCKER_CONTEXT:-}" ]; then
  endpoint=$(docker context inspect "$DOCKER_CONTEXT" --format '{{.Endpoints.docker.Host}}')
elif [ -n "${DOCKER_HOST:-}" ]; then
  endpoint=$DOCKER_HOST
else
  endpoint=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
fi
case "$endpoint" in
unix://*) ;;
*)
  echo 'Refusing cleanup: select a local Docker context using a Unix socket.' >&2
  exit 1
  ;;
esac
docker info >/dev/null

if [ -L .local ] || [ -L .local/cloud ] || [ -L .local/cloud/pki ]; then
  echo 'Refusing cleanup: local state must not redirect to another installation.' >&2
  exit 1
fi

# Preserve the CA and all data if uninstall fails or sudo is cancelled.
if [ -f .local/cloud/pki/ca.pem ]; then
  task cloud:local-uninstall
elif [ -f /etc/hosts ] && grep -q 'thermora-local:' /etc/hosts; then
  echo 'Local CA is missing but Thermora host entries remain. Restore the CA and run cloud:local-uninstall first.' >&2
  exit 1
fi

# Labels survive missing Compose files and include all named device pairs/orphans.
for project in thermora-cloud thermora; do
  containers=$(docker container ls -aq --filter "label=com.docker.compose.project=$project")
  if [ -n "$containers" ]; then
    # shellcheck disable=SC2086 # Docker emits whitespace-separated container IDs; globbing is disabled.
    docker container rm --force --volumes $containers
  fi
done

nodes=$(docker container ls -aq --filter 'name=^/floci-eks-thermora-cloud$' \
  --filter label=io.floci.service=eks --filter label=io.floci.resource-id=thermora-cloud)
if [ -n "$nodes" ]; then
  # --volumes removes the node's anonymous kubelet/CNI/log volumes as well.
  # shellcheck disable=SC2086 # Docker emits only container IDs; globbing is disabled.
  docker container rm --force --volumes $nodes
fi

for project in thermora-cloud thermora; do
  volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project")
  if [ -n "$volumes" ]; then
    # shellcheck disable=SC2086 # Docker volume names cannot contain whitespace; globbing is disabled.
    docker volume rm $volumes
  fi
  networks=$(docker network ls -q --filter "label=com.docker.compose.project=$project")
  if [ -n "$networks" ]; then
    # shellcheck disable=SC2086 # Docker emits only network IDs; globbing is disabled.
    docker network rm $networks
  fi
done

# K3s stores every PVC and its internal image store in this separate named volume.
volume=$(docker volume ls -q --filter 'name=^floci-eks-thermora-cloud$')
if [ -n "$volume" ]; then docker volume rm "$volume"; fi

for project in thermora-cloud thermora; do
  images=$(docker image ls -q --filter "label=com.docker.compose.project=$project")
  for image in $images; do
    # Never force image removal: another project's containers/tags may share it.
    users=$(docker container ls -aq --filter "ancestor=$image")
    if [ -n "$users" ]; then
      printf 'Keeping shared image %s: another container uses it.\n' "$image"
    else
      docker image rm "$image"
    fi
  done
done

# Credentials are removed last, so failed Docker cleanup can be retried.
rm -rf .local .task
echo 'Thermora runtime data removed. Run the README setup steps to start fresh.'
