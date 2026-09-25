// Package migrations embeds the versioned SQL migration files.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
