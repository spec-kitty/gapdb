//go:build !gapdb_crash_evidence

package main

import "gapdb/internal/faultfs"

func evidenceFS() faultfs.FS { return nil }
