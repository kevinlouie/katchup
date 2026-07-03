// Package migrations embeds the goose migration files so the binary
// can run them regardless of working directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
