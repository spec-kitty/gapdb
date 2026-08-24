package adoption

import (
	"context"
	"testing"
)

func TestResponseLossRequiresAmbiguityEffectAndAuthority(t *testing.T) {
	validRecord := Record{Key: "phase1/response-loss", Value: []byte("reconcile"), Revision: 7}
	validInspection := Inspection{DatabaseID: "database", CurrentRevision: 7, DurableThrough: 7, LiveRecords: 1}
	for name, backend := range map[string]responseLossProbeBackend{
		"valid-ambiguous-effect": {lossErr: ErrResponseAmbiguous, record: validRecord, inspection: validInspection},
		"ordinary-success":       {record: validRecord, inspection: validInspection},
		"no-apply":               {lossErr: ErrResponseAmbiguous, getErr: ErrNotFound, inspection: validInspection},
		"wrong-value":            {lossErr: ErrResponseAmbiguous, record: Record{Key: validRecord.Key, Value: []byte("wrong"), Revision: 7}, inspection: validInspection},
		"no-durable-authority":   {lossErr: ErrResponseAmbiguous, record: validRecord, inspection: Inspection{DatabaseID: "database", CurrentRevision: 7, DurableThrough: 6, LiveRecords: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			err := runResponseLoss(t.Context(), backend)
			if name == "valid-ambiguous-effect" {
				if err != nil {
					t.Fatalf("valid response loss failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid response loss passed")
			}
		})
	}
}

type responseLossProbeBackend struct {
	lossErr    error
	record     Record
	getErr     error
	inspection Inspection
}

func (responseLossProbeBackend) Name() string { return "probe" }
func (backend responseLossProbeBackend) Get(context.Context, string) (Record, error) {
	return backend.record, backend.getErr
}
func (responseLossProbeBackend) Put(context.Context, string, []byte, *Expiry, AckMode) (MutationResult, error) {
	return MutationResult{}, nil
}
func (responseLossProbeBackend) PutIfAbsent(context.Context, string, []byte, *Expiry, AckMode) (MutationResult, error) {
	return MutationResult{}, nil
}
func (responseLossProbeBackend) CompareAndSwap(context.Context, string, uint64, []byte, *Expiry, AckMode) (MutationResult, error) {
	return MutationResult{}, nil
}
func (responseLossProbeBackend) DeleteIfRevision(context.Context, string, uint64, AckMode) (MutationResult, error) {
	return MutationResult{}, nil
}
func (responseLossProbeBackend) AtomicBatch(context.Context, []BatchMutation, AckMode) (MutationResult, error) {
	return MutationResult{}, nil
}
func (responseLossProbeBackend) ScanPrefix(context.Context, string, int) ([]Record, error) {
	return nil, nil
}
func (responseLossProbeBackend) Watch(context.Context, string, uint64) (Watch, error) {
	return nil, nil
}
func (backend responseLossProbeBackend) PutWithLostResponse(context.Context, string, []byte, AckMode) error {
	return backend.lossErr
}
func (responseLossProbeBackend) Restart(context.Context) error { return nil }
func (backend responseLossProbeBackend) Inspect(context.Context) (Inspection, error) {
	return backend.inspection, nil
}
func (responseLossProbeBackend) Close() error { return nil }
