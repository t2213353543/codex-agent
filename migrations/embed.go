// Package migrations contains the immutable SQL migration source files.
package migrations

import "embed"

// Files contains the SQL files without a directory prefix. Database migration
// identities retain their historical "migrations/" prefix in the server.
//
//go:embed *.sql
var Files embed.FS
