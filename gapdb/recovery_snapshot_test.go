package gapdb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"strings"
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
	encodedValueOffset := bytes.Index(encoded, []byte("one"))
	if encodedValueOffset < 0 {
		t.Fatal("fixture value missing from frame")
	}
	encoded[encodedValueOffset] = 'X'
	if string(got.Records[0].Value) != "one" {
		t.Fatal("public decoder retained caller-owned frame bytes")
	}
	encoded[encodedValueOffset] = 'o'

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

func TestRecoverySnapshotCodecAdmitsEmptySnapshotAndRefusesExpiredMembership(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/v2/", ExpectedRevision: 9, MaxRecords: 2, MaxBytes: 1024}
	asOf := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	empty := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 9, AsOf: asOf}
	encoded, err := EncodeRecoverySnapshot(empty, request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecoverySnapshot(encoded, empty.RequestID, request)
	if err != nil || len(decoded.Records) != 0 {
		t.Fatalf("empty snapshot = %+v, %v", decoded, err)
	}

	expires := asOf
	expired := empty
	expired.Records = []Record{{Key: "spk/v2/expired", Value: []byte("value"), Revision: 8, ExpiresAt: &expires}}
	if _, err := EncodeRecoverySnapshot(expired, request); err == nil {
		t.Fatal("encoder admitted a record already expired at observation time")
	}

	tiny := request
	tiny.MaxBytes = 32
	if _, err := EncodeRecoverySnapshot(empty, tiny); err == nil {
		t.Fatal("empty snapshot header escaped the complete-frame byte budget")
	}
}

func TestRecoverySnapshotCodecAdmitsInitialRevision(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/v2/", MaxRecords: 1, MaxBytes: 1024}
	result := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", AsOf: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	payload, err := EncodeRecoverySnapshot(result, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRecoverySnapshot(payload, result.RequestID, request); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverySnapshotCodecSupportsHardKeyBoundAndRejectsInvalidUTF8(t *testing.T) {
	prefix := strings.Repeat("k", HardMaxKeyBytes-1)
	request := RecoverySnapshotRequest{Prefix: prefix, ExpectedRevision: 1, MaxRecords: 1, MaxBytes: 1 << 20}
	result := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 1, AsOf: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), Records: []Record{{Key: prefix + "x", Value: []byte("value"), Revision: 1}}}
	encoded, err := EncodeRecoverySnapshot(result, request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecoverySnapshot(encoded, result.RequestID, request)
	if err != nil || len(decoded.Records) != 1 || len(decoded.Records[0].Key) != HardMaxKeyBytes {
		t.Fatalf("hard-bound key roundtrip=%d err=%v", len(decoded.Records[0].Key), err)
	}
	invalid := request
	invalid.Prefix = string([]byte{'k', 0xff})
	if err := ValidateRecoverySnapshotRequest(invalid, Limits{MaxKeyBytes: HardMaxKeyBytes}); err == nil {
		t.Fatal("invalid UTF-8 prefix admitted")
	}
}

func TestPrivateRecoveryDecodeCapsOwnedValueSlices(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/", ExpectedRevision: 2, MaxRecords: 2, MaxBytes: 1024}
	result := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 2, AsOf: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), Records: []Record{{Key: "spk/a", Value: []byte("one"), Revision: 1}, {Key: "spk/b", Value: []byte("two"), Revision: 2}}}
	encoded, err := EncodeRecoverySnapshot(result, request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRecoverySnapshot(encoded, result.RequestID, request, true)
	if err != nil {
		t.Fatal(err)
	}
	for index := range decoded.Records {
		if cap(decoded.Records[index].Value) != len(decoded.Records[index].Value) {
			t.Fatalf("record %d value cap=%d len=%d", index, cap(decoded.Records[index].Value), len(decoded.Records[index].Value))
		}
	}
	first := append(decoded.Records[0].Value, 'x')
	if string(first) != "onex" || string(decoded.Records[1].Value) != "two" {
		t.Fatal("owned value append reached adjacent frame membership")
	}
}

func TestRecoverySnapshotSemanticMutantsFailAfterValidDigest(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/", ExpectedRevision: 7, MaxRecords: 1, MaxBytes: 1024}
	result := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 7, AsOf: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), Records: []Record{{Key: "spk/a", Value: []byte("one"), Revision: 7}}}
	encoded, err := EncodeRecoverySnapshot(result, request)
	if err != nil {
		t.Fatal(err)
	}
	identityEnd := 8 + 2 + len(result.DatabaseID) + 2 + len(result.RequestID)
	recordStart := identityEnd + 8 + 8 + 4
	mutants := map[string]func([]byte){
		"observed revision": func(value []byte) { binary.BigEndian.PutUint64(value[identityEnd:], 8) },
		"invalid key UTF-8": func(value []byte) { value[recordStart+4] = 0xff },
		"future record revision": func(value []byte) {
			keySize := int(binary.BigEndian.Uint32(value[recordStart:]))
			binary.BigEndian.PutUint64(value[recordStart+4+keySize:], 8)
		},
	}
	for name, mutate := range mutants {
		t.Run(name, func(t *testing.T) {
			candidate := bytes.Clone(encoded)
			mutate(candidate)
			digest := sha256.Sum256(candidate[:len(candidate)-sha256.Size])
			copy(candidate[len(candidate)-sha256.Size:], digest[:])
			if _, err := DecodeRecoverySnapshot(candidate, result.RequestID, request); err == nil {
				t.Fatal("semantic mutant passed with a valid frame digest")
			}
		})
	}
}

func TestRecoverySnapshotCodecRejectsUnrepresentableTimes(t *testing.T) {
	request := RecoverySnapshotRequest{Prefix: "spk/", ExpectedRevision: 1, MaxRecords: 1, MaxBytes: 1024}
	base := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 1, AsOf: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := EncodeRecoverySnapshot(base, request); err == nil {
		t.Fatal("unrepresentable observation time admitted")
	}
	base.AsOf = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	expires := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	base.Records = []Record{{Key: "spk/a", Value: []byte("one"), Revision: 1, ExpiresAt: &expires}}
	if _, err := EncodeRecoverySnapshot(base, request); err == nil {
		t.Fatal("unrepresentable expiry admitted")
	}
}

func FuzzDecodeRecoverySnapshot(f *testing.F) {
	request := RecoverySnapshotRequest{Prefix: "spk/", ExpectedRevision: 1, MaxRecords: 4, MaxBytes: 4096}
	result := RecoverySnapshotResult{DatabaseID: "database", RequestID: "request", ObservedRevision: 1, AsOf: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), Records: []Record{{Key: "spk/a", Value: []byte("one"), Revision: 1}}}
	encoded, err := EncodeRecoverySnapshot(result, request)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte("GDBREC1\n"))
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = DecodeRecoverySnapshot(payload, result.RequestID, request)
	})
}
