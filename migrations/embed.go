// Package migrations embeds the shipped schema so that the binary carries the exact SQL it will
// check the database against. There is no directory to forget to copy into an image, and no way
// for /readyz to compare a database to a schema other than the one this build was made from.
package migrations

import "embed"

// FS holds every shipped NNNN_name.sql. The Phase 2 draft in specs/002-… is deliberately absent:
// a table shipped before its feature exists could never be removed under a forward-only policy.
//
//go:embed *.sql
var FS embed.FS
