package gapdb

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateReadManyResultCrossFieldAndEncodedBounds(t *testing.T) {
	now := time.Date(2026, 9, 7, 17, 0, 0, 0, time.UTC)
	validRecord := NewRecord("key", []byte("value"), 2, nil)
	valid := ReadManyResult{ObservedRevision: 2, AsOf: now, Entries: []ReadManyEntry{{Key: "missing"}, {Key: "key", Found: true, Record: &validRecord}}}
	if err := ValidateReadManyResult(valid, DefaultOptions().Limits); err != nil {
		t.Fatal(err)
	}
	tests := map[string]ReadManyResult{
		"revision ahead": func() ReadManyResult {
			value := valid.Clone()
			value.Entries[1].Record.Revision = 3
			return value
		}(),
		"expired": func() ReadManyResult {
			value := valid.Clone()
			expiry := now
			value.Entries[1].Record.ExpiresAt = &expiry
			return value
		}(),
		"invalid absent key": {AsOf: now, Entries: []ReadManyEntry{{Key: strings.Repeat("k", HardMaxKeyBytes+1)}}},
		"presence mismatch":  {AsOf: now, Entries: []ReadManyEntry{{Key: "key", Found: true}}},
	}
	for name, result := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateReadManyResult(result, DefaultOptions().Limits); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}

	limits := DefaultOptions().Limits
	limits.MaxScanBytes = 160
	record := NewRecord("binary", make([]byte, 90), 1, nil)
	result := ReadManyResult{ObservedRevision: 1, AsOf: now, Entries: []ReadManyEntry{{Key: "binary", Found: true, Record: &record}}}
	if err := ValidateReadManyResult(result, limits); !errors.Is(err, &Error{Code: CodeFrameTooLarge}) {
		t.Fatalf("base64-expanded response error = %#v", err)
	}
}

func TestValidateManyRequestUsesExactEscapedJSONBytes(t *testing.T) {
	limits := DefaultOptions().Limits
	keys := []string{strings.Repeat("\"", 10)}
	limits.MaxBatchBytes = len(`{"keys":["`) + len(keys[0]) + len(`"]}`)
	if err := validateManyRequest(keys, limits); !errors.Is(err, &Error{Code: CodeBatchTooLarge}) {
		t.Fatalf("escaped exact request error = %#v", err)
	}
}
