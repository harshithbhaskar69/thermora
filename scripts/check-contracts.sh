#!/bin/sh
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
cd "$root"
sed "s|^output:.*|output: $scratch/api.gen.go|" api/oapi-codegen.yaml >"$scratch/generator.yaml"
go tool oapi-codegen -config "$scratch/generator.yaml" api/openapi.yaml
web/node_modules/.bin/openapi-typescript api/openapi.yaml -o "$scratch/schema.gen.ts"
cmp "$scratch/api.gen.go" internal/transport/http/generated/api.gen.go
cmp "$scratch/schema.gen.ts" web/src/api/schema.gen.ts
go test ./internal/cloud -run TestContractRejectsInvalidLoginBeforeDatabaseAccess
go test ./internal/protocol -run TestMQTTWireSchemas
