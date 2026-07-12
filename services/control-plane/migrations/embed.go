package migrations

import "embed"

// Files is the authoritative, versioned PostgreSQL DDL shipped with the control plane.
//
//go:embed *.sql
var Files embed.FS
