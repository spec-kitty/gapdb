package gapdb

import (
	"encoding/json"
	"fmt"
)

// ReadManyFrameReserveBytes bounds the unary envelope around a read-many
// result, including the maximum request ID and fixed database identity.
const ReadManyFrameReserveBytes = 1024

// ValidateReadManyResult verifies the cross-field snapshot and resource
// invariants shared by the engine, canonical protocol codec, and public client.
func ValidateReadManyResult(result ReadManyResult, limits Limits) error {
	if len(result.Entries) == 0 || len(result.Entries) > limits.MaxBatchOperations {
		return fmt.Errorf("read-many entry count is outside the configured limit")
	}
	if result.AsOf.IsZero() {
		return fmt.Errorf("read-many observation time is required")
	}
	_, offset := result.AsOf.Zone()
	if offset != 0 {
		return fmt.Errorf("read-many observation time must be UTC")
	}
	seen := make(map[string]struct{}, len(result.Entries))
	for _, entry := range result.Entries {
		if err := (Mutation{Kind: MutationPut, Key: entry.Key, Condition: Condition{Kind: ConditionAny}}).Validate(limits); err != nil {
			return fmt.Errorf("read-many entry key: %w", err)
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			return fmt.Errorf("read-many entry keys must be unique")
		}
		seen[entry.Key] = struct{}{}
		if entry.Found != (entry.Record != nil) {
			return fmt.Errorf("read-many found flag and record presence differ")
		}
		if entry.Record == nil {
			continue
		}
		record := entry.Record
		if record.Key != entry.Key || record.Revision == 0 || record.Revision > result.ObservedRevision {
			return fmt.Errorf("read-many record identity or revision is inconsistent")
		}
		if record.ExpiresAt != nil && !record.ExpiresAt.After(result.AsOf) {
			return fmt.Errorf("read-many record is expired at the observation time")
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode read-many result: %w", err)
	}
	maximum := limits.MaxScanBytes
	if frameMaximum := limits.MaxFrameBytes - ReadManyFrameReserveBytes; maximum <= 0 || frameMaximum < maximum {
		maximum = frameMaximum
	}
	if maximum <= 0 || len(encoded) > maximum {
		return &Error{Code: CodeFrameTooLarge, Message: "Exact read response exceeds the configured wire limit.", Retry: RetryNever, ReceivedBytes: len(encoded), MaximumBytes: maximum, SafeActions: []SafeAction{ActionReduceRequest, ActionAbort}}
	}
	return nil
}
