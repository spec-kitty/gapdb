package gapdb

import (
	"bytes"
	"testing"
	"time"
)

func TestRecoverySnapshotBinaryCodecRoundTripAndClosedMembership(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/v2/", ExpectedRevision: 42, MaxRecords: 3, MaxBytes: 1 << 20}
	expires := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	want := RecoverySnapshotResult{
		DatabaseID: "database-recovery", RequestID: "request-recovery", ObservedRevision: 42, AsOf: expires.Add(-time.Minute),
		Records: []Record{
			{Key: "spk/v2/a", Value: []byte("one"), Revision: 40},
			{Key: "spk/v2/b", Value: []byte("two"), Revision: 42, ExpiresAt: &expires},
		},
	}
	encoded, err := EncodeRecoverySnapshot(want, request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRecoverySnapshot(encoded, want.RequestID, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseID != want.DatabaseID || got.RequestID != want.RequestID || got.ObservedRevision != want.ObservedRevision || !got.AsOf.Equal(want.AsOf) || len(got.Records) != len(want.Records) {
		t.Fatalf("round trip differs: %+v", got)
	}
	for index := range got.Records {
		if got.Records[index].Key != want.Records[index].Key || got.Records[index].Revision != want.Records[index].Revision || !bytes.Equal(got.Records[index].Value, want.Records[index].Value) {
			t.Fatalf("record %d differs: %+v", index, got.Records[index])
		}
	}

	mutants := map[string]func([]byte){
		"digest": func(value []byte) { value[len(value)-1] ^= 1 },
		"body":   func(value []byte) { value[len(value)-33] ^= 1 },
	}
	for name, mutate := range mutants {
		t.Run(name, func(t *testing.T) {
			candidate := append([]byte(nil), encoded...)
			mutate(candidate)
			if _, err := DecodeRecoverySnapshot(candidate, want.RequestID, request); err == nil {
				t.Fatal("mutated recovery snapshot passed")
			}
		})
	}
	if _, err := DecodeRecoverySnapshot(encoded, "foreign-request", request); err == nil {
		t.Fatal("foreign request identity passed")
	}
	tooSmall := request
	tooSmall.MaxRecords = 1
	if _, err := DecodeRecoverySnapshot(encoded, want.RequestID, tooSmall); err == nil {
		t.Fatal("record budget substitution passed")
	}
}

func TestRecoverySnapshotCodecRefusesUnsortedForeignAndOversizedRecords(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/v2/", ExpectedRevision: 7, MaxRecords: 2, MaxBytes: 256}
	base := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 7, AsOf: time.Now().UTC(), Records: []Record{{Key: "spk/v2/b", Value: []byte("b"), Revision: 7}, {Key: "spk/v2/a", Value: []byte("a"), Revision: 7}}}
	if _, err := EncodeRecoverySnapshot(base, request); err == nil {
		t.Fatal("unsorted records passed")
	}
	base.Records = []Record{{Key: "foreign/a", Value: []byte("a"), Revision: 7}}
	if _, err := EncodeRecoverySnapshot(base, request); err == nil {
		t.Fatal("foreign prefix passed")
	}
	base.Records = []Record{{Key: "spk/v2/a", Value: bytes.Repeat([]byte("x"), 512), Revision: 7}}
	if _, err := EncodeRecoverySnapshot(base, request); err == nil {
		t.Fatal("oversized frame passed")
	}
}
