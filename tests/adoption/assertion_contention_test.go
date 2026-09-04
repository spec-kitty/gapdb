package adoption_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

const assertionContentionTrials = 1000

type assertionRaceHistory struct {
	name              string
	authorityKey      string
	targetKey         string
	condition         gapdb.Condition
	authorityRevision gapdb.Revision
	authorityRecord   gapdb.Record
	guardedRevision   gapdb.Revision
	guardedError      error
	targetRecord      gapdb.Record
	targetError       error
}

func (history assertionRaceHistory) valid() bool {
	if history.authorityRevision == 0 || history.authorityRecord.Key != history.authorityKey || history.authorityRecord.Revision != history.authorityRevision || string(history.authorityRecord.Value) != "generation-2" {
		return false
	}
	if history.guardedError == nil {
		return history.guardedRevision != 0 && history.guardedRevision < history.authorityRevision && history.targetError == nil && history.targetRecord.Key == history.targetKey && history.targetRecord.Revision == history.guardedRevision && string(history.targetRecord.Value) == "guarded"
	}
	var failure *gapdb.Error
	if !errors.As(history.guardedError, &failure) || failure.Code != gapdb.CodeConditionFailed || history.guardedRevision != 0 || failure.AssertionIndex == nil || *failure.AssertionIndex != 0 || failure.MutationIndex != nil || failure.Key != history.authorityKey || failure.Condition != string(history.condition.Kind) || failure.OperationApplied {
		return false
	}
	if history.condition.Kind == gapdb.ConditionRevision {
		if failure.ExpectedRevision == nil || *failure.ExpectedRevision != history.condition.ExpectedRevision || failure.ActualRevision == nil || *failure.ActualRevision != history.authorityRevision || failure.ActualState != "" {
			return false
		}
	} else if failure.ExpectedRevision != nil || failure.ActualRevision == nil || *failure.ActualRevision != history.authorityRevision || failure.ActualState != "" {
		return false
	}
	return errors.Is(history.targetError, &gapdb.Error{Code: gapdb.CodeNotFound}) && history.targetRecord.Revision == 0
}

func TestRealUnixStaleAuthorityContentionHasOnlyTwoValidHistories(t *testing.T) {
	backend := openGapdbBackend(t)
	defer backend.Close()
	second, err := gapdb.Dial(backend.server.SocketPath(), gapdb.ClientOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	revisionTrials, absenceTrials, guardedFirst, authorityFirst, staleEffects := 0, 0, 0, 0, 0
	for trial := 0; trial < assertionContentionTrials; trial++ {
		kind := "revision"
		if trial%2 == 1 {
			kind = "absence"
			absenceTrials++
		} else {
			revisionTrials++
		}
		name := fmt.Sprintf("trial-%04d/%s/seed-%016x", trial, kind, uint64(trial+1)*0x9e3779b97f4a7c15)
		authorityKey := fmt.Sprintf("assertion-race/%04d/authority", trial)
		targetKey := fmt.Sprintf("assertion-race/%04d/target", trial)
		condition := gapdb.Condition{Kind: gapdb.ConditionAbsent}
		if kind == "revision" {
			baseline, err := backend.client.Put(t.Context(), authorityKey, []byte("generation-1"), nil, gapdb.AckMemory)
			if err != nil {
				t.Fatalf("%s baseline: %v", name, err)
			}
			condition = gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: baseline.Revision}
		}

		start := make(chan struct{})
		authorityDone := make(chan struct {
			revision gapdb.Revision
			err      error
		}, 1)
		guardedDone := make(chan struct {
			revision gapdb.Revision
			err      error
		}, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			result, mutationErr := backend.client.Put(t.Context(), authorityKey, []byte("generation-2"), nil, gapdb.AckMemory)
			authorityDone <- struct {
				revision gapdb.Revision
				err      error
			}{result.Revision, mutationErr}
		}()
		go func() {
			defer wg.Done()
			<-start
			result, batchErr := second.AtomicBatch(t.Context(), gapdb.Batch{
				Ack:        gapdb.AckMemory,
				Assertions: []gapdb.Assertion{{Key: authorityKey, Condition: condition}},
				Mutations:  []gapdb.Mutation{gapdb.NewPutMutation(targetKey, []byte("guarded"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
			})
			guardedDone <- struct {
				revision gapdb.Revision
				err      error
			}{result.Revision, batchErr}
		}()
		close(start)
		wg.Wait()
		authority := <-authorityDone
		guarded := <-guardedDone
		if authority.err != nil {
			t.Fatalf("%s authority mutation: %v", name, authority.err)
		}
		authorityRecord, authorityGetErr := backend.client.Get(t.Context(), authorityKey)
		if authorityGetErr != nil {
			t.Fatalf("%s authority reconciliation: %v", name, authorityGetErr)
		}
		targetRecord, targetErr := backend.client.Get(t.Context(), targetKey)
		history := assertionRaceHistory{
			name: name, authorityKey: authorityKey, targetKey: targetKey, condition: condition,
			authorityRevision: authority.revision, authorityRecord: authorityRecord,
			guardedRevision: guarded.revision, guardedError: guarded.err,
			targetRecord: targetRecord, targetError: targetErr,
		}
		if !history.valid() {
			t.Fatalf("illegal serialization %s: authority=%d/%+v guarded=%d err=%v target=%+v/%v", history.name, history.authorityRevision, history.authorityRecord, history.guardedRevision, history.guardedError, history.targetRecord, history.targetError)
		}
		if guarded.err == nil {
			guardedFirst++
		} else {
			authorityFirst++
			if targetErr == nil {
				staleEffects++
			}
		}
	}
	if revisionTrials != 500 || absenceTrials != 500 || guardedFirst == 0 || authorityFirst == 0 || staleEffects != 0 || guardedFirst+authorityFirst != assertionContentionTrials {
		t.Fatalf("trial census revision=%d absence=%d guarded_first=%d authority_first=%d stale_effects=%d", revisionTrials, absenceTrials, guardedFirst, authorityFirst, staleEffects)
	}
	t.Logf("ASSERTION_CONTENTION_COUNTS=revision:%d absence:%d guarded_first:%d authority_first:%d stale_effects:%d", revisionTrials, absenceTrials, guardedFirst, authorityFirst, staleEffects)
}

func TestOutsideWriterPreReadMutantIsRejectedByTwoHistoryOracle(t *testing.T) {
	backend := openGapdbBackend(t)
	defer backend.Close()
	baseline, err := backend.client.Put(t.Context(), "outside-writer/authority", []byte("generation-1"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := backend.client.Get(t.Context(), "outside-writer/authority")
	if err != nil || observed.Revision != baseline.Revision {
		t.Fatalf("mutant pre-read = %+v, %v", observed, err)
	}
	changed, err := backend.client.Put(t.Context(), "outside-writer/authority", []byte("generation-2"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	// Controlled mutant: it trusts the earlier client-side read and omits the
	// assertion from the serialized request.
	guarded, err := backend.client.AtomicBatch(context.Background(), gapdb.Batch{
		Ack:       gapdb.AckMemory,
		Mutations: []gapdb.Mutation{gapdb.NewPutMutation("outside-writer/target", []byte("unsafe"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	})
	authorityRecord, getErr := backend.client.Get(t.Context(), "outside-writer/authority")
	if getErr != nil {
		t.Fatal(getErr)
	}
	targetRecord, targetErr := backend.client.Get(t.Context(), "outside-writer/target")
	mutant := assertionRaceHistory{
		name: "M-ASSERTION-OUTSIDE-WRITER", authorityKey: "outside-writer/authority", targetKey: "outside-writer/target",
		condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: observed.Revision}, authorityRevision: changed.Revision, authorityRecord: authorityRecord,
		guardedRevision: guarded.Revision, guardedError: err, targetRecord: targetRecord, targetError: targetErr,
	}
	if mutant.valid() {
		t.Fatalf("outside-writer mutant satisfied the two-history oracle: %+v", mutant)
	}

	_, err = backend.client.AtomicBatch(t.Context(), gapdb.Batch{
		Ack:        gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "outside-writer/authority", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: observed.Revision}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("outside-writer/safe-target", []byte("safe"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	})
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeConditionFailed}) {
		t.Fatalf("production stale assertion = %v", err)
	}
}

func TestAssertionContentionOracleKillsStateAndResultSubstitutions(t *testing.T) {
	authority := gapdb.Record{Key: "authority", Revision: 2, Value: []byte("generation-2")}
	target := gapdb.Record{Key: "target", Revision: 1, Value: []byte("guarded")}
	index, expected, actual := 0, gapdb.Revision(1), gapdb.Revision(2)
	failure := &gapdb.Error{
		Code: gapdb.CodeConditionFailed, AssertionIndex: &index, Key: "authority", Condition: string(gapdb.ConditionRevision),
		ExpectedRevision: &expected, ActualRevision: &actual,
	}
	base := assertionRaceHistory{
		name: "controlled", authorityKey: "authority", targetKey: "target",
		condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}, authorityRevision: 2, authorityRecord: authority,
	}
	t.Run("both_legal_histories", func(t *testing.T) {
		success := base
		success.guardedRevision, success.targetRecord = 1, target
		if !success.valid() {
			t.Fatal("guarded-first legal history rejected")
		}
		refusal := base
		refusal.guardedError, refusal.targetError = failure, &gapdb.Error{Code: gapdb.CodeNotFound}
		if !refusal.valid() {
			t.Fatal("authority-first legal history rejected")
		}
	})
	t.Run("mutate_then_error", func(t *testing.T) {
		mutant := base
		mutant.guardedError, mutant.targetRecord = failure, target
		if mutant.valid() {
			t.Fatal("mutate-then-error mutant survived")
		}
	})
	t.Run("success_without_publication", func(t *testing.T) {
		mutant := base
		mutant.guardedRevision, mutant.targetError = 1, &gapdb.Error{Code: gapdb.CodeNotFound}
		if mutant.valid() {
			t.Fatal("success-without-publication mutant survived")
		}
	})
}
