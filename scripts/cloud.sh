#!/bin/sh
# Explicit local cluster only; never reads or changes the user's kubectl context.
set -eu
umask 077
node=floci-eks-thermora-cloud
case "${1:-}" in
start)
  # Floci removes its node on shutdown. Describe triggers restoration from the
  # retained volume; wait for that before starting a survivor or checking K3s.
  for _ in $(seq 1 60); do
    if AWS_MAX_ATTEMPTS=1 sh scripts/cloud-aws.sh eks describe-cluster --name thermora-cloud \
      --cli-connect-timeout 2 --cli-read-timeout 5 >/dev/null 2>&1 &&
      docker start "$node" >/dev/null 2>&1 &&
      docker exec "$node" kubectl --request-timeout=5s get --raw=/readyz >/dev/null 2>&1; then
      exit 0
    fi
    sleep 1
  done
  echo 'Local Floci cluster did not become ready after 60 attempts. Inspect Floci logs before retrying.' >&2
  exit 1
  ;;
deploy)
  node_name=$(docker exec "$node" hostname)
  docker exec "$node" kubectl wait --for=create "node/$node_name" --timeout=120s
  docker exec "$node" kubectl wait --for=condition=Ready node --all --timeout=120s
  for image in \
    postgres:18.6-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873 \
    eclipse-mosquitto:2.0.22@sha256:199ea8ef2e35ec2b1b37e59cfd1dbae538ed4dfa4a2251a121a52215a6248a21; do
    docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
    docker tag "$image" "${image%@*}"
  done
  docker save thermora:local postgres:18.6-alpine eclipse-mosquitto:2.0.22 | docker exec -i "$node" ctr images import -
  docker exec "$node" kubectl create namespace thermora --dry-run=client -o yaml | docker exec -i "$node" kubectl apply -f -
  docker exec -i "$node" kubectl apply --server-side -f - <.local/cloud/secret.json
  docker exec "$node" kubectl -n thermora delete job migrate --ignore-not-found
  docker exec -i "$node" kubectl apply -f - <deploy/cloud/workloads.yaml
  floci_ip=$(docker inspect --format '{{(index .NetworkSettings.Networks "thermora-cloud-uplink").IPAddress}}' thermora-cloud-floci-1)
  docker exec -i "$node" kubectl apply -f - <<EOF
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: floci-local
  namespace: thermora
  labels:
    kubernetes.io/service-name: floci
    endpointslice.kubernetes.io/managed-by: thermora-local
addressType: IPv4
ports:
  - name: aws
    protocol: TCP
    port: 4566
endpoints:
  - addresses: ["$floci_ip"]
    conditions:
      ready: true
EOF
  docker exec "$node" kubectl -n thermora wait --for=condition=complete job/migrate --timeout=180s
  docker exec "$node" kubectl -n thermora rollout restart deployment/cloud
  docker exec "$node" kubectl -n thermora rollout status deployment/cloud --timeout=180s
  docker exec "$node" kubectl -n thermora exec deployment/cloud -c cloud -- thermora seed-onboarding
  for seed_path in .local/cloud/devices/*/onboarding.json; do
    if [ -f "$seed_path" ]; then
      docker exec -i "$node" kubectl -n thermora exec -i deployment/cloud -c cloud -- thermora seed-onboarding - <"$seed_path"
    fi
  done
  ;;
add-device)
  docker exec -i "$node" kubectl apply --server-side -f - <.local/cloud/secret.json
  docker exec "$node" kubectl -n thermora rollout restart deployment/cloud
  docker exec "$node" kubectl -n thermora rollout status deployment/cloud --timeout=180s
  docker exec -i "$node" kubectl -n thermora exec -i deployment/cloud -c cloud -- thermora seed-onboarding - <".local/cloud/devices/$2/onboarding.json"
  ;;
kubectl)
  shift
  exec docker exec "$node" kubectl --namespace=thermora "$@"
  ;;
stop)
  if docker container inspect "$node" >/dev/null 2>&1; then
    docker stop "$node"
  fi
  ;;
*)
  echo 'Usage: sh scripts/cloud.sh start | deploy | kubectl ARGS | stop' >&2
  exit 2
  ;;
esac
