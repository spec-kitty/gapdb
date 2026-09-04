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
	authorityRevision gapdb.Revision
	guardedRevision   gapdb.Revision
	guardedError      error
}

func (history assertionRaceHistory) valid() bool {
	if history.authorityRevision == 0 {
		return false
	}
	if history.guardedError == nil {
		return history.guardedRevision != 0 && history.guardedRevision < history.authorityRevision
	}
	var failure *gapdb.Error
	return errors.As(history.guardedError, &failure) && failure.Code == gapdb.CodeConditionFailed && history.guardedRevision == 0
}

func TestRealUnixStaleAuthorityContentionHasOnlyTwoValidHistories(t *testing.T) {
	backend := openGapdbBackend(t)
	defer backend.Close()
	second, err := gapdb.Dial(backend.server.SocketPath(), gapdb.ClientOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	revisionTrials, absenceTrials := 0, 0
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
		history := assertionRaceHistory{name: name, authorityRevision: authority.revision, guardedRevision: guarded.revision, guardedError: guarded.err}
		if !history.valid() {
			t.Fatalf("illegal serialization %s: authority=%d guarded=%d err=%v", history.name, history.authorityRevision, history.guardedRevision, history.guardedError)
		}
	}
	if revisionTrials != 500 || absenceTrials != 500 {
		t.Fatalf("trial census revision=%d absence=%d", revisionTrials, absenceTrials)
	}
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
	mutant := assertionRaceHistory{name: "M-ASSERTION-OUTSIDE-WRITER", authorityRevision: changed.Revision, guardedRevision: guarded.Revision, guardedError: err}
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
