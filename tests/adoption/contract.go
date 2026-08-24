// Package adoption defines the versioned, backend-neutral Phase 1 contract
// exported for the originating application's real SQLite and Gapdb adapters.
// It is test-only evidence and deliberately has no database implementation.
package adoption

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ContractVersion = "gapdb-phase1/v1"
	ScenarioCount   = 14
)

var (
	ErrNotFound          = errors.New("adoption backend: not found")
	ErrAlreadyExists     = errors.New("adoption backend: already exists")
	ErrRevisionMismatch  = errors.New("adoption backend: revision mismatch")
	ErrResponseAmbiguous = errors.New("adoption backend: response lost; outcome ambiguous")
)

type AckMode string

const (
	AckMemory  AckMode = "memory"
	AckDurable AckMode = "durable"
)

type Expiry struct {
	At time.Time
}

type Record struct {
	Key       string
	Value     []byte
	Revision  uint64
	ExpiresAt *Expiry
}

type MutationResult struct {
	Revision       uint64
	Ack            AckMode
	DurableThrough uint64
}

type BatchMutation struct {
	Key              string
	Value            []byte
	Delete           bool
	ExpectedRevision uint64
	RequireAbsent    bool
	ExpiresAt        *Expiry
}

type ChangeEvent struct {
	Revision uint64
	Order    uint32
	Key      string
	Deleted  bool
	Record   *Record
}

type Watch interface {
	RegistrationRevision() uint64
	Next(context.Context) (ChangeEvent, error)
	Close() error
}

type Inspection struct {
	DatabaseID      string
	CurrentRevision uint64
	DurableThrough  uint64
	LiveRecords     int
}

// Backend is intentionally behavioral: portable adapters expose no Gapdb or
// SQLite file/revision implementation details beyond opaque monotonic tokens.
type Backend interface {
	Name() string
	Get(context.Context, string) (Record, error)
	Put(context.Context, string, []byte, *Expiry, AckMode) (MutationResult, error)
	PutIfAbsent(context.Context, string, []byte, *Expiry, AckMode) (MutationResult, error)
	CompareAndSwap(context.Context, string, uint64, []byte, *Expiry, AckMode) (MutationResult, error)
	DeleteIfRevision(context.Context, string, uint64, AckMode) (MutationResult, error)
	AtomicBatch(context.Context, []BatchMutation, AckMode) (MutationResult, error)
	ScanPrefix(context.Context, string, int) ([]Record, error)
	Watch(context.Context, string, uint64) (Watch, error)
	// PutWithLostResponse executes a durable mutation through a backend's real
	// client transport, cuts the response, and returns only ErrResponseAmbiguous.
	// It deliberately exposes no revision or backend-specific fault mechanism.
	PutWithLostResponse(context.Context, string, []byte, AckMode) error
	Restart(context.Context) error
	Inspect(context.Context) (Inspection, error)
	Close() error
}

type ScenarioResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type ContractResult struct {
	SchemaVersion   uint16           `json:"schema_version"`
	ContractVersion string           `json:"contract_version"`
	Backend         string           `json:"backend"`
	Applicable      int              `json:"applicable"`
	Passed          bool             `json:"passed"`
	Scenarios       []ScenarioResult `json:"scenarios"`
}

type scenario struct {
	id  string
	run func(context.Context, Backend) error
}

func Run(ctx context.Context, backend Backend) ContractResult {
	result := ContractResult{SchemaVersion: 1, ContractVersion: ContractVersion, Backend: backend.Name()}
	for _, scenario := range contractScenarios() {
		outcome := ScenarioResult{ID: scenario.id, Status: "pass"}
		if err := scenario.run(ctx, backend); err != nil {
			outcome.Status = "fail"
			outcome.Error = boundedError(err)
		}
		result.Scenarios = append(result.Scenarios, outcome)
	}
	result.Applicable = len(result.Scenarios)
	result.Passed = result.Backend != "" && result.Applicable == ScenarioCount
	for _, outcome := range result.Scenarios {
		if outcome.Status != "pass" {
			result.Passed = false
		}
	}
	return result
}

func contractScenarios() []scenario {
	return []scenario{
		{"data/get-put", runGetPut},
		{"conditional/cas-delete", runConditional},
		{"batch/atomic", runBatch},
		{"scan/sorted-prefix", runScan},
		{"watch/registered-order", runWatch},
		{"expiry/release-authority", runExpiry},
		{"ack/memory-visible", runMemoryAck},
		{"ack/durable-restart", runDurableRestart},
		{"inspect/authority", runInspect},
		{"authority/lease", authorityScenario("lease")},
		{"authority/review", authorityScenario("review")},
		{"authority/workflow", authorityScenario("workflow")},
		{"authority/integration", authorityScenario("integration")},
		{"response-loss/reconcile", runResponseLoss},
	}
}

func runGetPut(ctx context.Context, backend Backend) error {
	result, err := backend.Put(ctx, "phase1/data", []byte{0, 1, 2, 255}, nil, AckMemory)
	if err != nil || result.Revision == 0 || result.Ack != AckMemory {
		return fmt.Errorf("memory put result=%+v: %w", result, err)
	}
	record, err := backend.Get(ctx, "phase1/data")
	if err != nil || record.Key != "phase1/data" || !bytes.Equal(record.Value, []byte{0, 1, 2, 255}) || record.Revision != result.Revision {
		return fmt.Errorf("opaque get record=%+v: %w", record, err)
	}
	return nil
}

func runConditional(ctx context.Context, backend Backend) error {
	winner, err := backend.PutIfAbsent(ctx, "phase1/conditional", []byte("one"), nil, AckDurable)
	if err := requireDurable(winner, err); err != nil {
		return err
	}
	if _, err := backend.PutIfAbsent(ctx, "phase1/conditional", []byte("loser"), nil, AckDurable); !errors.Is(err, ErrAlreadyExists) {
		return fmt.Errorf("put-if-absent loser=%v", err)
	}
	if _, err := backend.CompareAndSwap(ctx, "phase1/conditional", winner.Revision+1, []byte("bad"), nil, AckDurable); !errors.Is(err, ErrRevisionMismatch) {
		return fmt.Errorf("CAS mismatch=%v", err)
	}
	replaced, err := backend.CompareAndSwap(ctx, "phase1/conditional", winner.Revision, []byte("two"), nil, AckDurable)
	if err := requireDurable(replaced, err); err != nil {
		return err
	}
	if _, err := backend.DeleteIfRevision(ctx, "phase1/conditional", winner.Revision, AckDurable); !errors.Is(err, ErrRevisionMismatch) {
		return fmt.Errorf("delete mismatch=%v", err)
	}
	deleted, err := backend.DeleteIfRevision(ctx, "phase1/conditional", replaced.Revision, AckDurable)
	if err := requireDurable(deleted, err); err != nil {
		return err
	}
	if _, err := backend.Get(ctx, "phase1/conditional"); !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("deleted record remains: %v", err)
	}
	return nil
}

func runBatch(ctx context.Context, backend Backend) error {
	result, err := backend.AtomicBatch(ctx, []BatchMutation{
		{Key: "phase1/batch/a", Value: []byte("a"), RequireAbsent: true},
		{Key: "phase1/batch/b", Value: []byte("b"), RequireAbsent: true},
	}, AckDurable)
	if err := requireDurable(result, err); err != nil {
		return err
	}
	for _, key := range []string{"phase1/batch/a", "phase1/batch/b"} {
		record, err := backend.Get(ctx, key)
		if err != nil || record.Revision != result.Revision {
			return fmt.Errorf("atomic batch key=%q record=%+v: %w", key, record, err)
		}
	}
	return nil
}

func runScan(ctx context.Context, backend Backend) error {
	for _, key := range []string{"phase1/scan/b", "phase1/scan/a"} {
		if _, err := backend.Put(ctx, key, []byte(key), nil, AckMemory); err != nil {
			return err
		}
	}
	records, err := backend.ScanPrefix(ctx, "phase1/scan/", 10)
	if err != nil || len(records) != 2 || records[0].Key != "phase1/scan/a" || records[1].Key != "phase1/scan/b" {
		return fmt.Errorf("sorted scan=%+v: %w", records, err)
	}
	return nil
}

func runWatch(ctx context.Context, backend Backend) error {
	inspection, err := backend.Inspect(ctx)
	if err != nil {
		return err
	}
	watch, err := backend.Watch(ctx, "phase1/watch/", inspection.CurrentRevision)
	if err != nil || watch == nil || watch.RegistrationRevision() < inspection.CurrentRevision {
		return fmt.Errorf("watch registration: %w", err)
	}
	defer watch.Close()
	result, err := backend.Put(ctx, "phase1/watch/event", []byte("event"), nil, AckDurable)
	if err := requireDurable(result, err); err != nil {
		return err
	}
	nextCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	event, err := watch.Next(nextCtx)
	if err != nil || event.Key != "phase1/watch/event" || event.Revision != result.Revision || event.Record == nil || !bytes.Equal(event.Record.Value, []byte("event")) {
		return fmt.Errorf("watch event=%+v: %w", event, err)
	}
	return nil
}

func runExpiry(ctx context.Context, backend Backend) error {
	expiry := &Expiry{At: time.Now().Add(100 * time.Millisecond).UTC()}
	result, err := backend.Put(ctx, "phase1/expiry/lease", []byte("owner"), expiry, AckDurable)
	if err := requireDurable(result, err); err != nil {
		return err
	}
	if _, err := backend.Get(ctx, "phase1/expiry/lease"); err != nil {
		return fmt.Errorf("lease absent before expiry: %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := backend.Get(ctx, "phase1/expiry/lease"); errors.Is(err, ErrNotFound) {
			winner, err := backend.PutIfAbsent(ctx, "phase1/expiry/lease", []byte("next"), nil, AckDurable)
			return requireDurable(winner, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("expired lease continued authorizing its owner")
}

func runMemoryAck(ctx context.Context, backend Backend) error {
	result, err := backend.Put(ctx, "phase1/ack/memory", []byte("visible"), nil, AckMemory)
	if err != nil || result.Revision == 0 || result.Ack != AckMemory {
		return fmt.Errorf("memory acknowledgement=%+v: %w", result, err)
	}
	_, err = backend.Get(ctx, "phase1/ack/memory")
	return err
}

func runDurableRestart(ctx context.Context, backend Backend) error {
	result, err := backend.Put(ctx, "phase1/ack/durable", []byte("survives"), nil, AckDurable)
	if err := requireDurable(result, err); err != nil {
		return err
	}
	if err := backend.Restart(ctx); err != nil {
		return err
	}
	record, err := backend.Get(ctx, "phase1/ack/durable")
	if err != nil || !bytes.Equal(record.Value, []byte("survives")) {
		return fmt.Errorf("durable restart record=%+v: %w", record, err)
	}
	return nil
}

func runInspect(ctx context.Context, backend Backend) error {
	inspection, err := backend.Inspect(ctx)
	if err != nil || inspection.DatabaseID == "" || inspection.CurrentRevision == 0 || inspection.DurableThrough > inspection.CurrentRevision || inspection.LiveRecords <= 0 {
		return fmt.Errorf("inspection=%+v: %w", inspection, err)
	}
	return nil
}

func authorityScenario(kind string) func(context.Context, Backend) error {
	return func(ctx context.Context, backend Backend) error {
		key := "phase1/authority/" + kind
		winner, err := backend.PutIfAbsent(ctx, key, []byte("holder"), nil, AckDurable)
		if err := requireDurable(winner, err); err != nil {
			return err
		}
		if _, err := backend.PutIfAbsent(ctx, key, []byte("intruder"), nil, AckDurable); !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("%s authority admitted second owner: %v", kind, err)
		}
		if err := backend.Restart(ctx); err != nil {
			return err
		}
		record, err := backend.Get(ctx, key)
		if err != nil || !bytes.Equal(record.Value, []byte("holder")) {
			return fmt.Errorf("%s authority after restart=%+v: %w", kind, record, err)
		}
		return nil
	}
}

func runResponseLoss(ctx context.Context, backend Backend) error {
	if err := backend.PutWithLostResponse(ctx, "phase1/response-loss", []byte("reconcile"), AckDurable); !errors.Is(err, ErrResponseAmbiguous) {
		return fmt.Errorf("response loss did not produce stable ambiguity: %v", err)
	}
	if err := backend.Restart(ctx); err != nil {
		return err
	}
	record, err := backend.Get(ctx, "phase1/response-loss")
	if err != nil || record.Key != "phase1/response-loss" || record.Revision == 0 || !bytes.Equal(record.Value, []byte("reconcile")) {
		return fmt.Errorf("response-loss reconciliation=%+v: %w", record, err)
	}
	inspection, err := backend.Inspect(ctx)
	if err != nil || inspection.DatabaseID == "" || inspection.CurrentRevision < record.Revision || inspection.DurableThrough < record.Revision {
		return fmt.Errorf("response-loss authority=%+v record=%+v: %w", inspection, record, err)
	}
	return nil
}

func requireDurable(result MutationResult, err error) error {
	if err != nil {
		return err
	}
	if result.Revision == 0 || result.Ack != AckDurable || result.DurableThrough < result.Revision {
		return fmt.Errorf("durable result lacks authority: %+v", result)
	}
	return nil
}

func boundedError(err error) string {
	text := strings.ToValidUTF8(err.Error(), "�")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

func ScenarioIDs() []string {
	scenarios := contractScenarios()
	ids := make([]string, len(scenarios))
	for index, scenario := range scenarios {
		ids[index] = scenario.id
	}
	sort.Strings(ids)
	return ids
}
