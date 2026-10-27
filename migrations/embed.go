package migrations

import _ "embed"

// JobsSQL embeds the original queue schema migration.
//
//go:embed 001_jobs.sql
var JobsSQL string

// LeaseSQL embeds the controller lease and fencing migration.
//
//go:embed 002_lease.sql
var LeaseSQL string
