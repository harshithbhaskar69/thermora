// Package migrations embeds the ordered PostgreSQL schema.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
