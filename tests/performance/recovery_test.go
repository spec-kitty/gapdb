package performance_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/server"
)

func measureReferenceRecovery(t *testing.T, fixture *socketFixture) recoverySummary {
	t.Helper()
	client := fixture.clients[0]
	status, err := client.Status(t.Context())
	if err != nil || status.RecordCount != referenceRecordCount || status.CurrentRevision != referenceSnapshotRevision {
		t.Fatalf("pre-snapshot status=%+v err=%v", status, err)
	}
	snapshot, err := client.CreateSnapshot(t.Context(), status.DatabaseID, status.CurrentRevision)
	if err != nil || snapshot.Revision != status.CurrentRevision || snapshot.NewWALStart != snapshot.Revision+1 {
		t.Fatalf("snapshot=%+v status=%+v err=%v", snapshot, status, err)
	}

	var last gapdb.MutationResult
	for operation := range referenceLaterWALCommits {
		index := operation % referenceRecordCount
		ack := gapdb.AckMemory
		if operation == referenceLaterWALCommits-1 {
			ack = gapdb.AckDurable
		}
		last, err = fixture.put(t.Context(), client, recordKey(index), deterministicValue(referenceRecordCount+operation), ack)
		if err != nil {
			t.Fatalf("post-snapshot WAL commit %d: %v", operation, err)
		}
	}
	if last.Revision != snapshot.Revision+gapdb.Revision(referenceLaterWALCommits) || last.DurableThroughRevision < last.Revision {
		t.Fatalf("post-snapshot revision=%+v snapshot=%+v", last, snapshot)
	}
	directory := fixture.directory
	fixture.close(t)

	began := time.Now()
	restarted, err := server.Open(server.Config{
		Directory:    directory,
		Options:      gapdb.DefaultOptions(),
		ToolVersion:  "performance-recovery-evidence",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
		FS:           fixture.fs,
	})
	if err != nil {
		t.Fatalf("reopen snapshot plus WAL: %v", err)
	}
	defer restarted.Close(context.Background())
	reconciler, err := gapdb.Dial(restarted.SocketPath(), gapdb.ClientOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("dial recovered owner: %v", err)
	}
	defer reconciler.Close()
	recoveredStatus, err := reconciler.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	record, err := reconciler.Get(t.Context(), recordKey(referenceLaterWALCommits-1))
	ready := time.Since(began)
	if err != nil || recoveredStatus.RecordCount != referenceRecordCount || recoveredStatus.SnapshotRevision != snapshot.Revision || recoveredStatus.CurrentRevision != last.Revision || record.Revision <= snapshot.Revision || len(record.Value) != referenceValueBytes {
		t.Fatalf("recovered status=%+v record=%s@%d bytes=%d err=%v", recoveredStatus, record.Key, record.Revision, len(record.Value), err)
	}
	result := recoverySummary{
		SnapshotRevision: snapshot.Revision,
		LaterWALCommits:  referenceLaterWALCommits,
		ReadyMillis:      float64(ready) / float64(time.Millisecond),
		TargetMillis:     5_000,
	}
	result.TargetPassed = result.ReadyMillis <= result.TargetMillis
	return result
}

func TestRecoveryProfileConstantsAreLocked(t *testing.T) {
	if referenceRecordCount != 100_000 || referenceValueBytes != 1_024 || referenceLaterWALCommits != 10_000 {
		t.Fatalf("reference recovery workload drifted: records=%d bytes=%d commits=%d", referenceRecordCount, referenceValueBytes, referenceLaterWALCommits)
	}
	if got := fmt.Sprintf("%016x", referenceSeed); got != "4741504442504552" {
		t.Fatalf("reference seed drifted: %s", got)
	}
}
