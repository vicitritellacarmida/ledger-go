// Package migrations embeds the SQL schema migrations so the binary can apply
// them without shipping the .sql files separately.
package migrations

import "embed"

// FS holds every *.sql file in this directory. Files are applied in
// lexical order of their names, so always prefix them with a zero-padded
// sequence number such as 0004_.
//
//go:embed *.sql
var FS embed.FS
