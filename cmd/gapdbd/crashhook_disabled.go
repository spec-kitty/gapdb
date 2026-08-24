//go:build !gapdb_crash_evidence

package main

import "github.com/spec-kitty/gapdb/internal/faultfs"

func evidenceFS() faultfs.FS { return nil }
