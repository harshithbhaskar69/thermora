// Package api embeds the authoritative HTTP contract.
package api

import _ "embed"

//go:generate go tool oapi-codegen -config oapi-codegen.yaml openapi.yaml
//go:embed openapi.yaml
var OpenAPI []byte

//go:embed manufacturer.yaml
var ManufacturerOpenAPI []byte
