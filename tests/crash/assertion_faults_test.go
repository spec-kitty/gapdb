package crash_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

func TestAssertionPredicateCrashPointsAreZeroEffectAcrossRestart(t *testing.T) {
	binary := buildGapdbd(t)
	for _, point := range []string{"engine.assertion_evaluation", "engine.mutation_condition_evaluation"} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(point+"/"+string(phase), func(t *testing.T) {
				directory := filepath.Join(t.TempDir(), "database")
				seed := startCrashDaemon(t, binary, directory)
				client := dialCrashClient(t, seed.socket)
				guard, err := client.Put(t.Context(), "predicate/guard", []byte("authority"), nil, gapdb.AckDurable)
				if err != nil {
					t.Fatal(err)
				}
				_ = client.Close()
				seed.graceful(t)

				instance := startCrashDaemonWithEnvironment(t, binary, directory, map[string]string{
					"GAPDB_CRASH_POINT": point, "GAPDB_CRASH_PHASE": string(phase), "GAPDB_CRASH_OCCURRENCE": "1",
				})
				client = dialCrashClient(t, instance.socket)
				_, _ = client.AtomicBatch(context.Background(), gapdb.Batch{
					Ack:        gapdb.AckDurable,
					Assertions: []gapdb.Assertion{{Key: "predicate/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: guard.Revision}}},
					Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("predicate/target", []byte("must-not-appear"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
				})
				_ = client.Close()
				instance.waitForCrash(t)

				restarted := startCrashDaemon(t, binary, directory)
				reconciler := dialCrashClient(t, restarted.socket)
				gotGuard, err := reconciler.Get(t.Context(), "predicate/guard")
				if err != nil || gotGuard.Revision != guard.Revision || string(gotGuard.Value) != "authority" {
					t.Fatalf("guard after predicate crash = %+v, %v", gotGuard, err)
				}
				if target, err := reconciler.Get(t.Context(), "predicate/target"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
					t.Fatalf("predicate crash produced target %+v, %v", target, err)
				}
				status, err := reconciler.Status(t.Context())
				if err != nil || status.CurrentRevision != guard.Revision || status.DurableThroughRevision != guard.Revision {
					t.Fatalf("predicate crash advanced durable state: %+v, %v", status, err)
				}
				_ = reconciler.Close()
				restarted.graceful(t)
			})
		}
	}
}

func TestAssertionBearingCommitIsAtomicAtExistingCrashBoundaries(t *testing.T) {
	binary := buildGapdbd(t)
	points := []faultfs.Point{
		faultfs.PointWALFrameWrite,
		faultfs.PointWALBufferFlush,
		faultfs.PointWALFileSync,
		faultfs.PointMapApply,
		faultfs.PointResponsePublish,
	}
	for _, point := range points {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"/"+string(phase), func(t *testing.T) {
				directory := filepath.Join(t.TempDir(), "database")
				seed := startCrashDaemon(t, binary, directory)
				client := dialCrashClient(t, seed.socket)
				guard, err := client.Put(t.Context(), "commit/guard", []byte("authority"), nil, gapdb.AckDurable)
				if err != nil {
					t.Fatal(err)
				}
				_ = client.Close()
				seed.graceful(t)

				instance := startCrashDaemonWithEnvironment(t, binary, directory, map[string]string{
					"GAPDB_CRASH_POINT": string(point), "GAPDB_CRASH_PHASE": string(phase), "GAPDB_CRASH_OCCURRENCE": "1",
				})
				client = dialCrashClient(t, instance.socket)
				_, _ = client.AtomicBatch(context.Background(), gapdb.Batch{
					Ack:        gapdb.AckDurable,
					Assertions: []gapdb.Assertion{{Key: "commit/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: guard.Revision}}},
					Mutations: []gapdb.Mutation{
						gapdb.NewPutMutation("commit/a", []byte("a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
						gapdb.NewPutMutation("commit/b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
					},
				})
				_ = client.Close()
				instance.waitForCrash(t)

				restarted := startCrashDaemon(t, binary, directory)
				reconciler := dialCrashClient(t, restarted.socket)
				gotGuard, err := reconciler.Get(t.Context(), "commit/guard")
				if err != nil || gotGuard.Revision != guard.Revision || string(gotGuard.Value) != "authority" {
					t.Fatalf("asserted guard was mutated: %+v, %v", gotGuard, err)
				}
				a, aErr := reconciler.Get(t.Context(), "commit/a")
				b, bErr := reconciler.Get(t.Context(), "commit/b")
				aPresent := aErr == nil
				bPresent := bErr == nil
				if aPresent != bPresent {
					t.Fatalf("torn assertion-bearing commit: a=%+v/%v b=%+v/%v", a, aErr, b, bErr)
				}
				if aPresent {
					if a.Revision != b.Revision || a.Revision <= guard.Revision || string(a.Value) != "a" || string(b.Value) != "b" {
						t.Fatalf("recovered assertion-bearing commit mismatch: a=%+v b=%+v", a, b)
					}
				} else if !errors.Is(aErr, &gapdb.Error{Code: gapdb.CodeNotFound}) || !errors.Is(bErr, &gapdb.Error{Code: gapdb.CodeNotFound}) {
					t.Fatalf("unexpected recovery errors: a=%v b=%v", aErr, bErr)
				}
				_ = reconciler.Close()
				restarted.graceful(t)
			})
		}
	}
}
