// Package oceanbase embeds the fresh-install MySQL-mode schema for the OB PoC.
package oceanbase

import _ "embed"

const Version = 1

//go:embed 000001_baseline.up.sql
var Baseline string
