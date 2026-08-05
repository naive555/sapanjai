// Package migrations embeds the goose SQL files so the migrate command and
// tests can run migrations without the goose CLI or filesystem access.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
