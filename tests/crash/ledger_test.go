package crash_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
)

type acknowledgementClass string

const (
	acknowledgedDurable acknowledgementClass = "durable"
	acknowledgedMemory  acknowledgementClass = "memory"
	unacknowledged      acknowledgementClass = "unacknowledged"
)

type oracleEffect struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type oracleCommit struct {
	Schedule string               `json:"schedule"`
	Revision gapdb.Revision       `json:"revision"`
	Class    acknowledgementClass `json:"acknowledgement_class"`
	Effects  []oracleEffect       `json:"effects"`
}

type recoveryObservation struct {
	Revision        gapdb.Revision
	PostRevision    gapdb.Revision
	Records         map[string]string
	RecordRevisions map[string]gapdb.Revision
}

// reconcile is deliberately independent from Gapdb encoders and recovery. It
// represents only the externally promised acknowledgement rules.
func reconcile(commits []oracleCommit, recovered recoveryObservation) error {
	seen := make(map[gapdb.Revision]bool, len(commits))
	for _, commit := range commits {
		if commit.Revision == 0 || seen[commit.Revision] {
			return fmt.Errorf("revision %d is zero or reused", commit.Revision)
		}
		seen[commit.Revision] = true
		present := 0
		for _, effect := range commit.Effects {
			value, exists := recovered.Records[effect.Key]
			if !exists {
				continue
			}
			if value != effect.Value {
				return fmt.Errorf("schedule %s recovered key %q with a different value", commit.Schedule, effect.Key)
			}
			if recovered.RecordRevisions[effect.Key] != commit.Revision {
				return fmt.Errorf("schedule %s recovered key %q at revision %d instead of %d", commit.Schedule, effect.Key, recovered.RecordRevisions[effect.Key], commit.Revision)
			}
			present++
		}
		if present != 0 && present != len(commit.Effects) {
			return fmt.Errorf("schedule %s recovered a partial atomic commit", commit.Schedule)
		}
		if commit.Class == acknowledgedDurable && present != len(commit.Effects) {
			return fmt.Errorf("schedule %s lost durable acknowledgement", commit.Schedule)
		}
		if present == len(commit.Effects) && recovered.Revision < commit.Revision {
			return fmt.Errorf("schedule %s exposed revision beyond recovered authority", commit.Schedule)
		}
	}
	if recovered.PostRevision != 0 && recovered.PostRevision <= recovered.Revision {
		return fmt.Errorf("post-recovery revision %d did not advance recovered authority %d", recovered.PostRevision, recovered.Revision)
	}
	if recovered.PostRevision != 0 && seen[recovered.PostRevision] {
		return fmt.Errorf("post-recovery revision %d reused an attempted revision", recovered.PostRevision)
	}
	return nil
}

const processLedgerDomain = "GAPDB-PROCESS-LEDGER-V1"

type ledgerFrame struct {
	SchemaVersion  uint16          `json:"schema_version"`
	Sequence       uint64          `json:"sequence"`
	PreviousSHA256 string          `json:"previous_sha256"`
	Entry          json.RawMessage `json:"entry"`
	SHA256         string          `json:"sha256"`
}

type ledgerTerminal struct {
	SchemaVersion uint16 `json:"schema_version"`
	EntryCount    uint64 `json:"entry_count"`
	SHA256        string `json:"sha256"`
}

type ledger struct {
	file     *os.File
	path     string
	sequence uint64
	hash     [sha256.Size]byte
}

func openLedger(path string) (*ledger, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	result := &ledger{file: file, path: path}
	if err := result.installTerminal(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return result, nil
}

func (l *ledger) append(value any) error {
	entry, err := json.Marshal(value)
	if err != nil {
		return err
	}
	sequence := l.sequence + 1
	hash := processLedgerHash(l.hash, sequence, entry)
	frame := ledgerFrame{SchemaVersion: 1, Sequence: sequence, PreviousSHA256: hex.EncodeToString(l.hash[:]), Entry: entry, SHA256: hex.EncodeToString(hash[:])}
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := l.file.Write(line); err != nil {
		return err
	}
	if err := l.file.Sync(); err != nil {
		return err
	}
	l.sequence, l.hash = sequence, hash
	return l.installTerminal()
}

func (l *ledger) close() error { return l.file.Close() }

// terminal returns the writer's in-memory terminal evidence. The caller keeps
// this value outside the crash artifact directory and supplies it after the
// process under test has terminated.
func (l *ledger) terminal() ledgerTerminal {
	return ledgerTerminal{SchemaVersion: 1, EntryCount: l.sequence, SHA256: hex.EncodeToString(l.hash[:])}
}

func processLedgerHash(previous [sha256.Size]byte, sequence uint64, entry []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = io.WriteString(hash, processLedgerDomain)
	_, _ = hash.Write(previous[:])
	var encodedSequence [8]byte
	binary.BigEndian.PutUint64(encodedSequence[:], sequence)
	_, _ = hash.Write(encodedSequence[:])
	_, _ = hash.Write(entry)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func (l *ledger) installTerminal() error {
	terminal := l.terminal()
	encoded, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	digestPath := l.path + ".digest"
	temporary := fmt.Sprintf("%s.tmp-%d", digestPath, l.sequence)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, digestPath); err != nil {
		return err
	}
	removeTemporary = false
	directory, err := os.Open(filepath.Dir(l.path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func treeDigest(root string) (string, error) {
	hash := sha256.New()
	var names []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		names = append(names, relative)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, name)
		_, _ = fmt.Fprintf(hash, "\x00%d\x00", info.Mode())
		if info.Mode().IsRegular() {
			file, err := os.Open(filepath.Join(root, name))
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(hash, file); err != nil {
				_ = file.Close()
				return "", err
			}
			if err := file.Close(); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func TestAcknowledgementOracle(t *testing.T) {
	commits := []oracleCommit{
		{Schedule: "durable", Revision: 1, Class: acknowledgedDurable, Effects: []oracleEffect{{"a", "1"}, {"b", "1"}}},
		{Schedule: "memory", Revision: 2, Class: acknowledgedMemory, Effects: []oracleEffect{{"c", "2"}}},
		{Schedule: "response-lost", Revision: 3, Class: unacknowledged, Effects: []oracleEffect{{"d", "3"}}},
	}
	if err := reconcile(commits, recoveryObservation{Revision: 1, Records: map[string]string{"a": "1", "b": "1"}, RecordRevisions: map[string]gapdb.Revision{"a": 1, "b": 1}}); err != nil {
		t.Fatalf("legal recovery rejected: %v", err)
	}
	if err := reconcile(commits, recoveryObservation{Revision: 3, Records: map[string]string{"a": "1", "b": "1", "c": "2", "d": "3"}, RecordRevisions: map[string]gapdb.Revision{"a": 1, "b": 1, "c": 2, "d": 3}}); err != nil {
		t.Fatalf("legal complete recovery rejected: %v", err)
	}
	if err := reconcile(commits, recoveryObservation{Revision: 1, Records: map[string]string{"a": "1"}, RecordRevisions: map[string]gapdb.Revision{"a": 1}}); err == nil {
		t.Fatal("partial durable batch accepted")
	}
	if err := reconcile(commits, recoveryObservation{Revision: 0, Records: map[string]string{}}); err == nil {
		t.Fatal("lost durable acknowledgement accepted")
	}
	if err := reconcile(commits, recoveryObservation{Revision: 1, PostRevision: 3, Records: map[string]string{"a": "1", "b": "1"}, RecordRevisions: map[string]gapdb.Revision{"a": 1, "b": 1}}); err == nil {
		t.Fatal("post-recovery revision reuse accepted")
	}
	if err := reconcile(commits, recoveryObservation{Revision: 3, Records: map[string]string{"a": "1", "b": "1", "c": "2", "d": "3"}, RecordRevisions: map[string]gapdb.Revision{"a": 1, "b": 1, "c": 2, "d": 2}}); err == nil {
		t.Fatal("recovered value at the wrong revision accepted")
	}
}

func TestAcknowledgementOracleRejectsActualReservedRevisionReuse(t *testing.T) {
	const actualAttempted gapdb.Revision = 1_048_577
	commits := []oracleCommit{{Schedule: "response-lost", Revision: actualAttempted, Class: unacknowledged, Effects: []oracleEffect{{"target", "value"}}}}
	observation := recoveryObservation{Revision: 4, PostRevision: actualAttempted, Records: map[string]string{}, RecordRevisions: map[string]gapdb.Revision{}}
	if err := reconcile(commits, observation); err == nil || !strings.Contains(err.Error(), "reused an attempted revision") {
		t.Fatalf("real attempted revision reuse error=%v", err)
	}
}
