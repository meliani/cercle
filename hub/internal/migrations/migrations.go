// Package migrations embeds the numbered schema migrations (NNNN_name.sql).
package migrations

import "embed"

// FS holds every *.sql migration, applied in lexical (= numeric) order.
//
//go:embed *.sql
var FS embed.FS
