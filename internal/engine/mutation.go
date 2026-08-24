package engine

import (
	"context"
	"math"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

type CommitResult struct {
	gapdb.MutationResult
	Events []gapdb.ChangeEvent
}

type singleOperation uint8

const (
	operationBatch singleOperation = iota
	operationPut
	operationPutIfAbsent
	operationCompareAndSwap
	operationDeleteIfRevision
	operationExpiry
)

func (state *DatabaseState) Put(ctx context.Context, key string, value []byte, expiresAt *time.Time, ack gapdb.AckMode) (CommitResult, error) {
	return state.submitMutation(ctx, gapdb.NewPutMutation(key, value, gapdb.Condition{Kind: gapdb.ConditionAny}, expiresAt), ack, operationPut)
}

func (state *DatabaseState) PutIfAbsent(ctx context.Context, key string, value []byte, expiresAt *time.Time, ack gapdb.AckMode) (CommitResult, error) {
	return state.submitMutation(ctx, gapdb.NewPutMutation(key, value, gapdb.Condition{Kind: gapdb.ConditionAbsent}, expiresAt), ack, operationPutIfAbsent)
}

func (state *DatabaseState) CompareAndSwap(ctx context.Context, key string, expected gapdb.Revision, value []byte, expiresAt *time.Time, ack gapdb.AckMode) (CommitResult, error) {
	return state.submitMutation(ctx, gapdb.NewPutMutation(key, value, gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: expected}, expiresAt), ack, operationCompareAndSwap)
}

func (state *DatabaseState) DeleteIfRevision(ctx context.Context, key string, expected gapdb.Revision, ack gapdb.AckMode) (CommitResult, error) {
	return state.submitMutation(ctx, gapdb.NewDeleteMutation(key, expected), ack, operationDeleteIfRevision)
}

func (state *DatabaseState) submitMutation(ctx context.Context, mutation gapdb.Mutation, ack gapdb.AckMode, operation singleOperation) (CommitResult, error) {
	return state.prepareAndSubmit(ctx, gapdb.Batch{Ack: ack, Mutations: []gapdb.Mutation{mutation}}, operation)
}

func (state *DatabaseState) AtomicBatch(ctx context.Context, batch gapdb.Batch) (CommitResult, error) {
	return state.prepareAndSubmit(ctx, batch, operationBatch)
}

func (state *DatabaseState) prepareAndSubmit(ctx context.Context, batch gapdb.Batch, operation singleOperation) (CommitResult, error) {
	batch = batch.Clone()
	if err := validateBatchAt(batch, state.limits, state.clock.Now()); err != nil {
		return CommitResult{}, err
	}
	return state.submit(ctx, batch, operation)
}

func validateBatchAt(batch gapdb.Batch, limits gapdb.Limits, now time.Time) error {
	if err := batch.ValidateAt(limits, now); err != nil {
		return err
	}
	total := uint64(32)
	maximum := limits.MaxBatchBytes
	if limits.MaxFrameBytes < maximum {
		maximum = limits.MaxFrameBytes
	}
	for _, mutation := range batch.Mutations {
		if mutation.ExpiresAt != nil && !expiryRepresentable(*mutation.ExpiresAt) {
			return invalidField("expires_at", "must be representable as signed Unix nanoseconds")
		}
		addition := uint64(20) + uint64(len(mutation.Key)) + uint64(len(mutation.Value))
		if addition > math.MaxUint64-total {
			return batchTooLarge(math.MaxInt, maximum)
		}
		total += addition
	}
	if total > uint64(maximum) || total > math.MaxUint32 {
		received := int(total)
		if total > math.MaxInt {
			received = math.MaxInt
		}
		return batchTooLarge(received, maximum)
	}
	return nil
}

func expiryRepresentable(value time.Time) bool {
	value = value.UTC()
	return time.Unix(0, value.UnixNano()).UTC().Equal(value)
}

func batchTooLarge(received, maximum int) error {
	return &gapdb.Error{Code: gapdb.CodeBatchTooLarge, Message: "Batch exceeds the configured byte limit.", Retry: gapdb.RetryNever, ReceivedBytes: received, MaximumBytes: maximum, SafeActions: []gapdb.SafeAction{gapdb.ActionSplitBatch, gapdb.ActionAbort}}
}
