package adoption_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/protocol"
	"gapdb/internal/server"
	"gapdb/tests/adoption"
)

func TestGapdbAdapterPassesAllApplicableScenarios(t *testing.T) {
	backend := openGapdbBackend(t)
	defer backend.Close()
	result := adoption.Run(t.Context(), backend)
	if !result.Passed || result.Applicable != adoption.ScenarioCount {
		for _, scenario := range result.Scenarios {
			if scenario.Status != "pass" {
				t.Logf("failed scenario %s: %s", scenario.ID, scenario.Error)
			}
		}
		t.Fatalf("Gapdb portable contract failed: %+v", result)
	}
}

func TestPortableContractRejectsVacuousBackend(t *testing.T) {
	if adoption.ContractVersion != "gapdb-phase1/v1" {
		t.Fatalf("contract version=%q", adoption.ContractVersion)
	}
	result := adoption.Run(t.Context(), vacuousBackend{})
	if result.Passed || result.Applicable == 0 || len(result.Scenarios) != adoption.ScenarioCount {
		t.Fatalf("vacuous backend passed or skipped the contract: %+v", result)
	}
}

func TestResponseLossScenarioRejectsOrdinarySuccess(t *testing.T) {
	backend := ordinaryResponseBackend{vacuousBackend: vacuousBackend{}}
	result := adoption.Run(t.Context(), backend)
	for _, scenario := range result.Scenarios {
		if scenario.ID == "response-loss/reconcile" {
			if scenario.Status != "fail" || scenario.Error == "" {
				t.Fatalf("ordinary successful response passed response-loss scenario: %+v", scenario)
			}
			return
		}
	}
	t.Fatal("response-loss scenario absent")
}

type ordinaryResponseBackend struct{ vacuousBackend }

func (ordinaryResponseBackend) Put(context.Context, string, []byte, *adoption.Expiry, adoption.AckMode) (adoption.MutationResult, error) {
	return adoption.MutationResult{Revision: 7, Ack: adoption.AckDurable, DurableThrough: 7}, nil
}

func (ordinaryResponseBackend) Get(context.Context, string) (adoption.Record, error) {
	return adoption.Record{Key: "phase1/response-loss", Value: []byte("reconcile"), Revision: 7}, nil
}

func (ordinaryResponseBackend) PutWithLostResponse(context.Context, string, []byte, adoption.AckMode) error {
	return nil
}

type vacuousBackend struct{}

func (vacuousBackend) Name() string { return "vacuous" }
func (vacuousBackend) Get(context.Context, string) (adoption.Record, error) {
	return adoption.Record{}, nil
}
func (vacuousBackend) Put(context.Context, string, []byte, *adoption.Expiry, adoption.AckMode) (adoption.MutationResult, error) {
	return adoption.MutationResult{}, nil
}
func (vacuousBackend) PutIfAbsent(context.Context, string, []byte, *adoption.Expiry, adoption.AckMode) (adoption.MutationResult, error) {
	return adoption.MutationResult{}, nil
}
func (vacuousBackend) CompareAndSwap(context.Context, string, uint64, []byte, *adoption.Expiry, adoption.AckMode) (adoption.MutationResult, error) {
	return adoption.MutationResult{}, nil
}
func (vacuousBackend) DeleteIfRevision(context.Context, string, uint64, adoption.AckMode) (adoption.MutationResult, error) {
	return adoption.MutationResult{}, nil
}
func (vacuousBackend) AtomicBatch(context.Context, []adoption.BatchMutation, adoption.AckMode) (adoption.MutationResult, error) {
	return adoption.MutationResult{}, nil
}
func (vacuousBackend) ScanPrefix(context.Context, string, int) ([]adoption.Record, error) {
	return nil, nil
}
func (vacuousBackend) Watch(context.Context, string, uint64) (adoption.Watch, error) {
	return nil, nil
}
func (vacuousBackend) PutWithLostResponse(context.Context, string, []byte, adoption.AckMode) error {
	return nil
}
func (vacuousBackend) Restart(context.Context) error { return nil }
func (vacuousBackend) Inspect(context.Context) (adoption.Inspection, error) {
	return adoption.Inspection{}, nil
}
func (vacuousBackend) Close() error { return nil }

type gapdbBackend struct {
	directory string
	server    *server.Server
	client    *gapdb.Client
}

func openGapdbBackend(t *testing.T) *gapdbBackend {
	t.Helper()
	backend := &gapdbBackend{directory: filepath.Join(t.TempDir(), "database")}
	if err := backend.start(); err != nil {
		t.Fatal(err)
	}
	return backend
}

func (backend *gapdbBackend) start() error {
	instance, err := server.Open(server.Config{Directory: backend.directory, Options: gapdb.DefaultOptions(), ToolVersion: "adoption-contract", ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second})
	if err != nil {
		return err
	}
	client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 5 * time.Second})
	if err != nil {
		_ = instance.Close(context.Background())
		return err
	}
	backend.server, backend.client = instance, client
	return nil
}

func (*gapdbBackend) Name() string { return "gapdb" }

func (backend *gapdbBackend) Get(ctx context.Context, key string) (adoption.Record, error) {
	record, err := backend.client.Get(ctx, key)
	if err != nil {
		return adoption.Record{}, portableError(err)
	}
	return portableRecord(record), nil
}

func (backend *gapdbBackend) Put(ctx context.Context, key string, value []byte, expiry *adoption.Expiry, ack adoption.AckMode) (adoption.MutationResult, error) {
	result, err := backend.client.Put(ctx, key, value, expiryTime(expiry), gapdbAck(ack))
	return portableMutation(result), portableError(err)
}

func (backend *gapdbBackend) PutIfAbsent(ctx context.Context, key string, value []byte, expiry *adoption.Expiry, ack adoption.AckMode) (adoption.MutationResult, error) {
	result, err := backend.client.PutIfAbsent(ctx, key, value, expiryTime(expiry), gapdbAck(ack))
	return portableMutation(result), portableError(err)
}

func (backend *gapdbBackend) CompareAndSwap(ctx context.Context, key string, revision uint64, value []byte, expiry *adoption.Expiry, ack adoption.AckMode) (adoption.MutationResult, error) {
	result, err := backend.client.CompareAndSwap(ctx, key, gapdb.Revision(revision), value, expiryTime(expiry), gapdbAck(ack))
	return portableMutation(result), portableError(err)
}

func (backend *gapdbBackend) DeleteIfRevision(ctx context.Context, key string, revision uint64, ack adoption.AckMode) (adoption.MutationResult, error) {
	result, err := backend.client.DeleteIfRevision(ctx, key, gapdb.Revision(revision), gapdbAck(ack))
	return portableMutation(result), portableError(err)
}

func (backend *gapdbBackend) AtomicBatch(ctx context.Context, mutations []adoption.BatchMutation, ack adoption.AckMode) (adoption.MutationResult, error) {
	batch := gapdb.Batch{Ack: gapdbAck(ack), Mutations: make([]gapdb.Mutation, 0, len(mutations))}
	for _, mutation := range mutations {
		if mutation.Delete {
			batch.Mutations = append(batch.Mutations, gapdb.NewDeleteMutation(mutation.Key, gapdb.Revision(mutation.ExpectedRevision)))
			continue
		}
		condition := gapdb.Condition{Kind: gapdb.ConditionAny}
		if mutation.RequireAbsent {
			condition.Kind = gapdb.ConditionAbsent
		} else if mutation.ExpectedRevision != 0 {
			condition = gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: gapdb.Revision(mutation.ExpectedRevision)}
		}
		batch.Mutations = append(batch.Mutations, gapdb.NewPutMutation(mutation.Key, mutation.Value, condition, expiryTime(mutation.ExpiresAt)))
	}
	result, err := backend.client.AtomicBatch(ctx, batch)
	return portableMutation(result), portableError(err)
}

func (backend *gapdbBackend) ScanPrefix(ctx context.Context, prefix string, limit int) ([]adoption.Record, error) {
	page, err := backend.client.ScanPrefix(ctx, prefix, limit, "")
	if err != nil {
		return nil, portableError(err)
	}
	result := make([]adoption.Record, len(page.Records))
	for index, record := range page.Records {
		result[index] = portableRecord(record)
	}
	return result, nil
}

func (backend *gapdbBackend) Watch(ctx context.Context, prefix string, after uint64) (adoption.Watch, error) {
	watch, err := backend.client.Watch(ctx, prefix, gapdb.Revision(after))
	if err != nil {
		return nil, portableError(err)
	}
	return &portableWatch{watch: watch}, nil
}

func (backend *gapdbBackend) PutWithLostResponse(ctx context.Context, key string, value []byte, ack adoption.AckMode) error {
	if ack != adoption.AckDurable {
		return errors.New("response-loss seam requires durable acknowledgement")
	}
	before, err := backend.client.Status(ctx)
	if err != nil {
		return err
	}
	request := protocol.Request{
		SchemaVersion: protocol.SchemaVersion,
		Operation:     protocol.OperationPut,
		Arguments: protocol.PutArguments{
			Key: key, Value: protocol.Base64Bytes(append([]byte(nil), value...)), Ack: gapdb.AckDurable,
		},
	}
	payload, err := protocol.EncodeRequest(request, gapdb.DefaultOptions().Limits)
	if err != nil {
		return err
	}
	connection, err := net.DialTimeout("unix", backend.server.SocketPath(), 5*time.Second)
	if err != nil {
		return err
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return errors.New("response-loss seam did not use a Unix connection")
	}
	if err := protocol.WriteFrame(unixConnection, payload, gapdb.DefaultMaxFrameBytes); err != nil {
		_ = unixConnection.Close()
		return err
	}
	// Cut the response direction before the owner can publish the result. Keep
	// the write side alive while a separate public client observes only that the
	// request was processed; no response or revision is decoded here.
	if err := unixConnection.CloseRead(); err != nil {
		_ = unixConnection.Close()
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, statusErr := backend.client.Status(ctx)
		if statusErr == nil && status.CurrentRevision > before.CurrentRevision {
			break
		}
		if time.Now().After(deadline) {
			_ = unixConnection.Close()
			return fmt.Errorf("lost-response request was not accepted before deadline: %w", statusErr)
		}
		time.Sleep(time.Millisecond)
	}
	_ = unixConnection.Close()
	return adoption.ErrResponseAmbiguous
}

func (backend *gapdbBackend) Restart(ctx context.Context) error {
	if backend.client != nil {
		_ = backend.client.Close()
		backend.client = nil
	}
	if backend.server != nil {
		if err := backend.server.Close(ctx); err != nil {
			return err
		}
		backend.server = nil
	}
	return backend.start()
}

func (backend *gapdbBackend) Inspect(ctx context.Context) (adoption.Inspection, error) {
	status, err := backend.client.Status(ctx)
	if err != nil {
		return adoption.Inspection{}, portableError(err)
	}
	return adoption.Inspection{DatabaseID: status.DatabaseID, CurrentRevision: uint64(status.CurrentRevision), DurableThrough: uint64(status.DurableThroughRevision), LiveRecords: status.RecordCount}, nil
}

func (backend *gapdbBackend) Close() error {
	if backend.client != nil {
		_ = backend.client.Close()
		backend.client = nil
	}
	if backend.server != nil {
		err := backend.server.Close(context.Background())
		backend.server = nil
		return err
	}
	return nil
}

type portableWatch struct{ watch *gapdb.Watch }

func (watch *portableWatch) RegistrationRevision() uint64 {
	return uint64(watch.watch.RegistrationRevision)
}

func (watch *portableWatch) Next(ctx context.Context) (adoption.ChangeEvent, error) {
	select {
	case <-ctx.Done():
		return adoption.ChangeEvent{}, ctx.Err()
	case event, open := <-watch.watch.Events:
		if !open {
			return adoption.ChangeEvent{}, errors.New("watch event stream closed")
		}
		result := adoption.ChangeEvent{Revision: uint64(event.Revision), Order: event.Order, Key: event.Key, Deleted: event.Record == nil}
		if event.Record != nil {
			record := portableRecord(*event.Record)
			result.Record = &record
		}
		return result, nil
	case ended := <-watch.watch.Ended:
		if ended.Error != nil {
			return adoption.ChangeEvent{}, portableError(ended.Error)
		}
		return adoption.ChangeEvent{}, fmt.Errorf("watch ended: %s", ended.Reason)
	}
}

func (watch *portableWatch) Close() error {
	watch.watch.Close()
	return nil
}

func portableRecord(record gapdb.Record) adoption.Record {
	result := adoption.Record{Key: record.Key, Value: append([]byte(nil), record.Value...), Revision: uint64(record.Revision)}
	if record.ExpiresAt != nil {
		result.ExpiresAt = &adoption.Expiry{At: *record.ExpiresAt}
	}
	return result
}

func portableMutation(result gapdb.MutationResult) adoption.MutationResult {
	return adoption.MutationResult{Revision: uint64(result.Revision), Ack: adoption.AckMode(result.Ack), DurableThrough: uint64(result.DurableThroughRevision)}
}

func expiryTime(expiry *adoption.Expiry) *time.Time {
	if expiry == nil {
		return nil
	}
	result := expiry.At.UTC()
	return &result
}

func gapdbAck(ack adoption.AckMode) gapdb.AckMode { return gapdb.AckMode(ack) }

func portableError(err error) error {
	if err == nil {
		return nil
	}
	for code, portable := range map[gapdb.ErrorCode]error{
		gapdb.CodeNotFound:         adoption.ErrNotFound,
		gapdb.CodeAlreadyExists:    adoption.ErrAlreadyExists,
		gapdb.CodeRevisionMismatch: adoption.ErrRevisionMismatch,
	} {
		if errors.Is(err, &gapdb.Error{Code: code}) {
			return fmt.Errorf("%w: %v", portable, err)
		}
	}
	return err
}
