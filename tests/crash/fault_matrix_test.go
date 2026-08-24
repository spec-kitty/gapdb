package crash_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
	"github.com/spec-kitty/gapdb/internal/server"
)

type faultBoundary struct {
	Category string
	Point    faultfs.Point
}

var faultBoundaries = []faultBoundary{
	{"ownership", faultfs.PointLockOpen},
	{"identity", faultfs.PointIdentityTempCreate}, {"identity", faultfs.PointIdentityTempWrite}, {"identity", faultfs.PointIdentityTempSync}, {"identity", faultfs.PointIdentityRename}, {"identity", faultfs.PointIdentityDirectorySync},
	{"wal", faultfs.PointWALHeaderCreate}, {"wal", faultfs.PointWALHeaderWrite}, {"wal", faultfs.PointWALFrameWrite}, {"wal", faultfs.PointWALBufferFlush}, {"wal", faultfs.PointWALFileSync}, {"recovery", faultfs.PointWALTailTruncate}, {"recovery", faultfs.PointWALTailSync},
	{"snapshot", faultfs.PointSnapshotTempCreate}, {"snapshot", faultfs.PointSnapshotTempWrite}, {"snapshot", faultfs.PointSnapshotTempSync}, {"snapshot", faultfs.PointSnapshotRename}, {"snapshot", faultfs.PointSnapshotDirectorySync},
	{"snapshot", faultfs.PointNextWALCreate}, {"snapshot", faultfs.PointNextWALSync}, {"snapshot", faultfs.PointNextWALRename},
	{"manifest", faultfs.PointManifestTempCreate}, {"manifest", faultfs.PointManifestTempWrite}, {"manifest", faultfs.PointManifestTempSync}, {"manifest", faultfs.PointManifestRename}, {"manifest", faultfs.PointManifestDirectorySync},
	{"compaction", faultfs.PointCompactionRemove}, {"compaction", faultfs.PointCompactionDirectorySync},
	{"backup", faultfs.PointBackupFileCopy}, {"backup", faultfs.PointBackupFileSync}, {"backup", faultfs.PointBackupRename}, {"backup", faultfs.PointPublicationDirectorySync},
	{"audit", faultfs.PointAuditAppend}, {"audit", faultfs.PointAuditSync},
	{"apply", faultfs.PointMapApply}, {"response", faultfs.PointResponsePublish},
	{"recovery", faultfs.PointOpen}, {"recovery", faultfs.PointStat}, {"recovery", faultfs.PointReadDir},
}

type namedSchedule struct {
	Name           string
	Seed           uint64
	Boundary       faultBoundary
	Phase          faultfs.Phase
	RequestedClass acknowledgementClass
}

func schedules(count int) []namedSchedule {
	result := make([]namedSchedule, count)
	classes := []acknowledgementClass{acknowledgedDurable, acknowledgedMemory, unacknowledged}
	for index := range result {
		boundary := faultBoundaries[index%len(faultBoundaries)]
		cycle := index / len(faultBoundaries)
		phase := faultfs.Before
		if cycle%2 == 1 {
			phase = faultfs.After
		}
		class := classes[(index+cycle)%len(classes)]
		seed := uint64(index+1)*0x9e3779b97f4a7c15 ^ 0x4741504442464155
		result[index] = namedSchedule{Name: fmt.Sprintf("seed-%016x/%s/%s/%s/occurrence-1", seed, boundary.Point, phase, class), Seed: seed, Boundary: boundary, Phase: phase, RequestedClass: class}
	}
	return result
}

func TestAcceptanceDeterministicFaultSchedules(t *testing.T) {
	count := 1024
	if testing.Short() {
		count = 128
	}
	matrix := schedules(count)
	if !testing.Short() && len(matrix) < 1000 {
		t.Fatalf("acceptance profile executed %d schedules, want at least 1000", len(matrix))
	}
	root := t.TempDir()
	coverage := make(map[string]int)
	classCounts := map[acknowledgementClass]int{}
	injectedClassCounts := map[acknowledgementClass]int{}
	for index, schedule := range matrix {
		directory := filepath.Join(root, fmt.Sprintf("schedule-%04d", index))
		result, err := runProductionSchedule(directory, schedule)
		if err != nil {
			t.Fatalf("smallest reproducer: %s: %v", schedule.Name, err)
		}
		if !result.HookObserved {
			t.Fatalf("smallest reproducer: %s: product path skipped declared hook", schedule.Name)
		}
		coverage[string(schedule.Boundary.Point)+"/"+string(schedule.Phase)]++
		for _, commit := range result.Commits {
			classCounts[commit.Class]++
		}
		if result.InjectedCommit != nil {
			injectedClassCounts[result.InjectedCommit.Class]++
		}
		if err := reconcile(result.Commits, result.Recovered); err != nil {
			t.Fatalf("smallest reproducer: %s: %v", schedule.Name, err)
		}
		if !result.RecoveryAvailable && !result.FailClosedVerified {
			t.Fatalf("smallest reproducer: %s: neither public recovery nor stable fail-closed evidence: %v", schedule.Name, result.RecoveryError)
		}
		if err := os.RemoveAll(directory); err != nil {
			t.Fatalf("cleanup %s: %v", schedule.Name, err)
		}
	}
	for _, class := range []acknowledgementClass{acknowledgedDurable, acknowledgedMemory, unacknowledged} {
		if classCounts[class] == 0 {
			t.Fatalf("response-derived acknowledgement class %s has zero schedules", class)
		}
		if !testing.Short() && injectedClassCounts[class] == 0 {
			t.Fatalf("injected response-derived acknowledgement class %s has zero schedules", class)
		}
	}
	for _, boundary := range faultBoundaries {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			if coverage[string(boundary.Point)+"/"+string(phase)] == 0 {
				t.Fatalf("missing hook coverage: %s/%s", boundary.Point, phase)
			}
		}
	}
	t.Logf("fault_evidence schedules=%d seed_first=%d seed_last=%d named_hooks=%d durable=%d memory=%d unacknowledged=%d injected_durable=%d injected_memory=%d injected_unacknowledged=%d", len(matrix), matrix[0].Seed, matrix[len(matrix)-1].Seed, len(coverage), classCounts[acknowledgedDurable], classCounts[acknowledgedMemory], classCounts[unacknowledged], injectedClassCounts[acknowledgedDurable], injectedClassCounts[acknowledgedMemory], injectedClassCounts[unacknowledged])
}

type productionScheduleResult struct {
	HookObserved        bool
	Commits             []oracleCommit
	Recovered           recoveryObservation
	RecoveryAvailable   bool
	RecoveryError       error
	FailClosedVerified  bool
	InjectedCommit      *oracleCommit
	ReservedRevisionEnd gapdb.Revision
	EvidenceError       error
}

func runProductionSchedule(directory string, schedule namedSchedule) (productionScheduleResult, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return productionScheduleResult{}, err
	}
	var observed atomic.Bool
	injector := faultfs.NewInjector(schedule.Seed, faultfs.Rule{Point: schedule.Boundary.Point, Phase: schedule.Phase, Occurrence: 1, Seed: schedule.Seed, Err: errors.New("deterministic injected stop"), Observe: func(faultfs.Event) { observed.Store(true) }})
	injectedFS := faultfs.NewOS(injector)
	result := productionScheduleResult{}
	point := schedule.Boundary.Point

	switch schedule.Boundary.Category {
	case "ownership", "identity":
		instance, _ := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
		if instance != nil {
			_ = instance.Close(context.Background())
		}
		result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
	case "wal":
		if point == faultfs.PointWALHeaderCreate || point == faultfs.PointWALHeaderWrite {
			instance, _ := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
			if instance != nil {
				_ = instance.Close(context.Background())
			}
			result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
			break
		}
		if err := addBaseline(directory, &result); err != nil {
			return result, err
		}
		result = runInjectedMutation(directory, schedule, injectedFS, result)
	case "apply", "response":
		if err := addBaseline(directory, &result); err != nil {
			return result, err
		}
		result = runInjectedMutation(directory, schedule, injectedFS, result)
	case "recovery":
		if err := addBaseline(directory, &result); err != nil {
			return result, err
		}
		if point == faultfs.PointWALTailTruncate || point == faultfs.PointWALTailSync {
			if err := appendTornTail(directory); err != nil {
				return result, err
			}
		}
		if point == faultfs.PointStat {
			if err := os.WriteFile(filepath.Join(directory, "audit.jsonl"), nil, 0o600); err != nil {
				return result, err
			}
		}
		instance, _ := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
		if instance != nil {
			if point == faultfs.PointStat {
				client, dialErr := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
				if dialErr == nil {
					status, statusErr := client.Status(context.Background())
					if statusErr == nil {
						_, _ = client.CreateSnapshot(context.Background(), status.DatabaseID, status.CurrentRevision)
					}
					_ = client.Close()
				}
			}
			_ = instance.Close(context.Background())
		}
		result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
	case "snapshot", "manifest", "audit":
		if err := addBaseline(directory, &result); err != nil {
			return result, err
		}
		instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
		if err != nil {
			return result, err
		}
		client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
		if err != nil {
			_ = instance.Close(context.Background())
			return result, err
		}
		status, statusErr := client.Status(context.Background())
		if statusErr == nil {
			_, _ = client.CreateSnapshot(context.Background(), status.DatabaseID, status.CurrentRevision)
		}
		_ = client.Close()
		_ = instance.Close(context.Background())
		result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
	case "compaction":
		commits, _, err := initializeBaselineAndSnapshot(directory)
		if err != nil {
			return result, err
		}
		result.Commits = append(result.Commits, commits...)
		instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
		if err != nil {
			return result, err
		}
		client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
		if err == nil {
			status, statusErr := client.Status(context.Background())
			if statusErr == nil {
				_, _ = client.Compact(context.Background(), status.DatabaseID, status.SnapshotRevision)
			}
			_ = client.Close()
		}
		_ = instance.Close(context.Background())
		result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
	case "backup":
		if err := addBaseline(directory, &result); err != nil {
			return result, err
		}
		instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
		if err != nil {
			return result, err
		}
		client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
		if err == nil {
			status, statusErr := client.Status(context.Background())
			if statusErr == nil {
				_, _ = client.Backup(context.Background(), status.DatabaseID, status.DurableThroughRevision, filepath.Join(filepath.Dir(directory), filepath.Base(directory)+"-backup"))
			}
			_ = client.Close()
		}
		_ = instance.Close(context.Background())
		result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
	default:
		return result, fmt.Errorf("unhandled production category %q", schedule.Boundary.Category)
	}
	result.HookObserved = observed.Load()
	if result.EvidenceError != nil {
		return result, result.EvidenceError
	}
	if !result.RecoveryAvailable {
		result.FailClosedVerified = verifyStableFailClosed(directory, result.RecoveryError)
	}
	return result, nil
}

func addBaseline(directory string, result *productionScheduleResult) error {
	commits, reservedRevisionEnd, err := initializeBaseline(directory)
	if err == nil {
		result.Commits = append(result.Commits, commits...)
		result.ReservedRevisionEnd = reservedRevisionEnd
	}
	return err
}

func initializeBaseline(directory string) ([]oracleCommit, gapdb.Revision, error) {
	instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance"})
	if err != nil {
		return nil, 0, err
	}
	client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
	if err != nil {
		_ = instance.Close(context.Background())
		return nil, 0, err
	}
	commits := make([]oracleCommit, 0, 4)
	durable, err := client.Put(context.Background(), "baseline/durable", []byte("durable"), nil, gapdb.AckDurable)
	if err == nil {
		commits = append(commits, oracleCommit{Schedule: "baseline/durable-response", Revision: durable.Revision, Class: classifyAcknowledgement(gapdb.AckDurable, err), Effects: []oracleEffect{{"baseline/durable", "durable"}}})
	}
	memory, memoryErr := client.Put(context.Background(), "baseline/memory", []byte("memory"), nil, gapdb.AckMemory)
	if err == nil && memoryErr == nil {
		commits = append(commits, oracleCommit{Schedule: "baseline/memory-response", Revision: memory.Revision, Class: classifyAcknowledgement(gapdb.AckMemory, memoryErr), Effects: []oracleEffect{{"baseline/memory", "memory"}}})
	}
	conditionRevision := memory.Revision
	_, conditionErr := client.PutIfAbsent(context.Background(), "baseline/memory", []byte("loser"), nil, gapdb.AckDurable)
	if err == nil && memoryErr == nil && !errors.Is(conditionErr, &gapdb.Error{Code: gapdb.CodeAlreadyExists}) {
		err = fmt.Errorf("baseline condition loser: %w", conditionErr)
	}
	status, statusErr := client.Status(context.Background())
	if err == nil && memoryErr == nil && (statusErr != nil || status.CurrentRevision != conditionRevision) {
		err = fmt.Errorf("condition failure consumed revision: status=%d want=%d: %w", status.CurrentRevision, conditionRevision, statusErr)
	}
	expires := time.Now().Add(time.Hour).UTC()
	expiry, expiryErr := client.Put(context.Background(), "baseline/expiry", []byte("expiring"), &expires, gapdb.AckDurable)
	if err == nil && memoryErr == nil && expiryErr == nil {
		commits = append(commits, oracleCommit{Schedule: "baseline/expiry-response", Revision: expiry.Revision, Class: classifyAcknowledgement(gapdb.AckDurable, expiryErr), Effects: []oracleEffect{{"baseline/expiry", "expiring"}}})
	}
	batch, batchErr := client.AtomicBatch(context.Background(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("baseline/batch-a", []byte("batch-a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		gapdb.NewPutMutation("baseline/batch-b", []byte("batch-b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
	}})
	if err == nil && memoryErr == nil && expiryErr == nil && batchErr == nil {
		commits = append(commits, oracleCommit{Schedule: "baseline/atomic-batch-response", Revision: batch.Revision, Class: classifyAcknowledgement(gapdb.AckDurable, batchErr), Effects: []oracleEffect{{"baseline/batch-a", "batch-a"}, {"baseline/batch-b", "batch-b"}}})
	}
	_ = client.Close()
	closeErr := instance.Close(context.Background())
	if err != nil {
		return nil, 0, err
	}
	if memoryErr != nil {
		return nil, 0, memoryErr
	}
	if expiryErr != nil {
		return nil, 0, expiryErr
	}
	if batchErr != nil {
		return nil, 0, batchErr
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	return commits, status.ReservedRevisionEnd, nil
}

func initializeBaselineAndSnapshot(directory string) ([]oracleCommit, gapdb.Revision, error) {
	commits, reservedRevisionEnd, err := initializeBaseline(directory)
	if err != nil {
		return nil, 0, err
	}
	instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance"})
	if err != nil {
		return nil, 0, err
	}
	client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
	if err == nil {
		status, statusErr := client.Status(context.Background())
		if statusErr == nil {
			_, err = client.CreateSnapshot(context.Background(), status.DatabaseID, status.CurrentRevision)
		} else {
			err = statusErr
		}
		_ = client.Close()
	}
	closeErr := instance.Close(context.Background())
	if err != nil {
		return nil, 0, err
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	return commits, reservedRevisionEnd, nil
}

func classifyAcknowledgement(ack gapdb.AckMode, responseErr error) acknowledgementClass {
	if responseErr != nil {
		return unacknowledged
	}
	if ack == gapdb.AckMemory {
		return acknowledgedMemory
	}
	return acknowledgedDurable
}

func TestSuccessfulResponseRevisionBindsToReservedBoundary(t *testing.T) {
	maximum := ^gapdb.Revision(0)
	for _, test := range []struct {
		name        string
		reservedEnd gapdb.Revision
		response    gapdb.Revision
		want        gapdb.Revision
		wantError   bool
	}{
		{name: "exact", reservedEnd: 1_048_576, response: 1_048_577, want: 1_048_577},
		{name: "zero", reservedEnd: 1_048_576, response: 0, wantError: true},
		{name: "lower", reservedEnd: 1_048_576, response: 1_048_576, wantError: true},
		{name: "higher", reservedEnd: 1_048_576, response: 1_048_578, wantError: true},
		{name: "maximum-equality", reservedEnd: maximum - 1, response: maximum, want: maximum},
		{name: "overflow-adjacent-higher", reservedEnd: maximum - 2, response: maximum, wantError: true},
		{name: "overflow-boundary", reservedEnd: maximum, response: maximum, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := bindSuccessfulResponseRevision(test.reservedEnd, test.response)
			if test.wantError {
				if err == nil || got != 0 {
					t.Fatalf("bind=(%d, %v), want zero and error", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("bind=(%d, %v), want (%d, nil)", got, err, test.want)
			}
		})
	}
}

func bindSuccessfulResponseRevision(reservedEnd, response gapdb.Revision) (gapdb.Revision, error) {
	if reservedEnd == 0 || reservedEnd == ^gapdb.Revision(0) {
		return 0, fmt.Errorf("pre-operation reserved revision boundary %d is invalid", reservedEnd)
	}
	attempted := reservedEnd + 1
	if response != attempted {
		return 0, fmt.Errorf("response revision=%d does not match durable attempted revision=%d", response, attempted)
	}
	return response, nil
}

func runInjectedMutation(directory string, schedule namedSchedule, injectedFS faultfs.FS, result productionScheduleResult) productionScheduleResult {
	instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-acceptance", FS: injectedFS})
	if err == nil {
		client, dialErr := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
		if dialErr == nil {
			if result.ReservedRevisionEnd == 0 || result.ReservedRevisionEnd == ^gapdb.Revision(0) {
				_ = client.Close()
				_ = instance.Close(context.Background())
				result.EvidenceError = fmt.Errorf("pre-operation reserved revision boundary %d is invalid", result.ReservedRevisionEnd)
				return result
			}
			attemptedRevision := result.ReservedRevisionEnd + 1
			ack := gapdb.AckMemory
			if schedule.RequestedClass != acknowledgedMemory || schedule.Boundary.Point == faultfs.PointWALBufferFlush || schedule.Boundary.Point == faultfs.PointWALFileSync {
				ack = gapdb.AckDurable
			}
			mutation, mutationErr := client.AtomicBatch(context.Background(), gapdb.Batch{Ack: ack, Mutations: []gapdb.Mutation{
				gapdb.NewPutMutation("target/a", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
				gapdb.NewPutMutation("target/b", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			}})
			class := classifyAcknowledgement(ack, mutationErr)
			revision := attemptedRevision
			if mutationErr == nil {
				revision, result.EvidenceError = bindSuccessfulResponseRevision(result.ReservedRevisionEnd, mutation.Revision)
			}
			commit := oracleCommit{Schedule: schedule.Name, Revision: revision, Class: class, Effects: []oracleEffect{{"target/a", "value"}, {"target/b", "value"}}}
			result.Commits = append(result.Commits, commit)
			result.InjectedCommit = &commit
			_ = client.Close()
		}
		_ = instance.Close(context.Background())
	}
	result.Recovered, result.RecoveryAvailable, result.RecoveryError = observeRecovered(directory, result.Commits)
	return result
}

func observeRecovered(directory string, commits []oracleCommit) (recoveryObservation, bool, error) {
	observation := recoveryObservation{Records: make(map[string]string), RecordRevisions: make(map[string]gapdb.Revision)}
	instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-observer"})
	if err != nil {
		return observation, false, err
	}
	defer instance.Close(context.Background())
	client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
	if err != nil {
		return observation, false, err
	}
	defer client.Close()
	status, err := client.Status(context.Background())
	if err != nil {
		return observation, false, err
	}
	observation.Revision = status.CurrentRevision
	keys := make(map[string]bool)
	for _, commit := range commits {
		for _, effect := range commit.Effects {
			keys[effect.Key] = true
		}
	}
	for key := range keys {
		record, getErr := client.Get(context.Background(), key)
		if getErr == nil {
			observation.Records[key] = string(record.Value)
			observation.RecordRevisions[key] = record.Revision
		} else if !errors.Is(getErr, &gapdb.Error{Code: gapdb.CodeNotFound}) {
			return observation, false, getErr
		}
	}
	post, err := client.Put(context.Background(), "post/recovery", []byte("verified"), nil, gapdb.AckDurable)
	if err != nil || post.Revision <= observation.Revision {
		return observation, false, fmt.Errorf("post-recovery commit revision=%d after=%d: %w", post.Revision, observation.Revision, err)
	}
	observation.PostRevision = post.Revision
	if err := client.Close(); err != nil {
		return observation, false, err
	}
	if err := instance.Close(context.Background()); err != nil {
		return observation, false, err
	}
	restarted, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-post-observer"})
	if err != nil {
		return observation, false, err
	}
	postClient, err := gapdb.Dial(restarted.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second})
	if err != nil {
		_ = restarted.Close(context.Background())
		return observation, false, err
	}
	postRecord, err := postClient.Get(context.Background(), "post/recovery")
	_ = postClient.Close()
	_ = restarted.Close(context.Background())
	if err != nil || postRecord.Revision != post.Revision || string(postRecord.Value) != "verified" {
		return observation, false, fmt.Errorf("post-recovery restart evidence=%+v: %w", postRecord, err)
	}
	return observation, true, nil
}

func verifyStableFailClosed(directory string, first error) bool {
	var structured *gapdb.Error
	if !errors.As(first, &structured) || len(structured.SafeActions) == 0 {
		return false
	}
	switch structured.Code {
	case gapdb.CodeRecoveryRequired, gapdb.CodeIOError, gapdb.CodeCorruptIdentity, gapdb.CodeCorruptManifest, gapdb.CodeCorruptWAL:
	default:
		return false
	}
	before, err := treeDigest(directory)
	if err != nil {
		return false
	}
	_, available, repeated := observeRecovered(directory, nil)
	if available {
		return false
	}
	var repeatedStructured *gapdb.Error
	if !errors.As(repeated, &repeatedStructured) || repeatedStructured.Code != structured.Code {
		return false
	}
	after, err := treeDigest(directory)
	return err == nil && before == after
}

func appendTornTail(directory string) error {
	fsys := faultfs.NewOS(nil)
	identity, err := persist.ReadIdentity(fsys, directory)
	if err != nil {
		return err
	}
	manifest, err := persist.ReadManifest(fsys, directory, identity.DatabaseID, 1)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(directory, manifest.WALFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write([]byte{0, 0, 0}); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func TestProductionApplyAndResponseFaultSeamsRecoverWholeCommit(t *testing.T) {
	for _, point := range []faultfs.Point{faultfs.PointMapApply, faultfs.PointResponsePublish} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"/"+string(phase), func(t *testing.T) {
				directory := t.TempDir()
				injector := faultfs.NewInjector(71, faultfs.Rule{Point: point, Phase: phase, Occurrence: 1, Seed: 71, Err: errors.New("crash boundary")})
				instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-matrix", FS: faultfs.NewOS(injector)})
				if err != nil {
					t.Fatal(err)
				}
				client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr := client.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
					gapdb.NewPutMutation("atomic/a", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
					gapdb.NewPutMutation("atomic/b", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
				}})
				if mutationErr == nil && !(point == faultfs.PointResponsePublish && phase == faultfs.After) {
					t.Fatal("injected production fault returned success")
				}
				_ = client.Close()
				_ = instance.Close(context.Background())

				restarted, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "fault-matrix"})
				if err != nil {
					t.Fatalf("restart: %v", err)
				}
				defer restarted.Close(context.Background())
				reconciler, err := gapdb.Dial(restarted.SocketPath(), gapdb.ClientOptions{Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer reconciler.Close()
				for _, key := range []string{"atomic/a", "atomic/b"} {
					record, getErr := reconciler.Get(t.Context(), key)
					if getErr != nil || string(record.Value) != "value" {
						t.Fatalf("%s recovery = %+v, %v; want whole replayable batch", key, record, getErr)
					}
				}
			})
		}
	}
}

func TestFaultOracleUsesActualDurableAttemptedRevision(t *testing.T) {
	schedule := namedSchedule{
		Name:           "actual-attempted-revision",
		Seed:           97,
		Boundary:       faultBoundary{Category: "apply", Point: faultfs.PointMapApply},
		Phase:          faultfs.Before,
		RequestedClass: acknowledgedDurable,
	}
	result, err := runProductionSchedule(t.TempDir(), schedule)
	if err != nil {
		t.Fatal(err)
	}
	actualAttempted := result.ReservedRevisionEnd + 1
	if result.InjectedCommit == nil || actualAttempted != 1_048_577 || result.InjectedCommit.Revision != actualAttempted {
		t.Fatalf("reserved_end=%d attempted=%d commit=%+v", result.ReservedRevisionEnd, actualAttempted, result.InjectedCommit)
	}
	if result.Recovered.PostRevision != 2_097_153 {
		t.Fatalf("post-recovery revision=%d, want next durable reservation start 2097153", result.Recovered.PostRevision)
	}
	for _, key := range []string{"target/a", "target/b"} {
		if result.Recovered.RecordRevisions[key] != actualAttempted {
			t.Fatalf("public recovery key %q revision=%d, want actual attempted %d", key, result.Recovered.RecordRevisions[key], actualAttempted)
		}
	}
	adversarial := recoveryObservation{Revision: 4, PostRevision: actualAttempted, Records: make(map[string]string), RecordRevisions: make(map[string]gapdb.Revision)}
	for _, commit := range result.Commits[:len(result.Commits)-1] {
		for _, effect := range commit.Effects {
			adversarial.Records[effect.Key] = effect.Value
			adversarial.RecordRevisions[effect.Key] = commit.Revision
		}
	}
	if err := reconcile(result.Commits, adversarial); err == nil || !strings.Contains(err.Error(), "reused an attempted revision") {
		t.Fatalf("official oracle accepted real attempted revision reuse: %v", err)
	}
}
