package server_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
	"gapdb/internal/protocol"
	"gapdb/internal/server"
	"golang.org/x/sys/unix"
)

func TestOpenServesPublicClientAndOwnsSocket(t *testing.T) {
	dir := t.TempDir()
	srv, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })

	info, err := os.Stat(filepath.Join(dir, "gapdb.sock"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info, err)
	}
	client, err := gapdb.Dial(filepath.Join(dir, "gapdb.sock"), gapdb.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	put, err := client.Put(t.Context(), "key", []byte{0, 1, 2}, nil, gapdb.AckDurable)
	if err != nil || put.Revision == 0 || put.DurableThroughRevision < put.Revision {
		t.Fatalf("Put = %+v, %v", put, err)
	}
	record, err := client.Get(t.Context(), "key")
	if err != nil || string(record.Value) != "\x00\x01\x02" || record.Revision != put.Revision {
		t.Fatalf("Get = %+v, %v", record, err)
	}

	if _, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		t.Fatalf("second owner = %v, want OWNER_EXISTS", err)
	}
}

func TestReviewerAdminResultsUseLockedWireShapes(t *testing.T) {
	dir := t.TempDir()
	srv, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Put(t.Context(), "admin/key", []byte("value"), nil, gapdb.AckDurable); err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(t.Context())
	if err != nil || status.OwnerPID == 0 || status.OwnerStartedAt == "" || status.DatabaseID == "" || status.Limits.MaxFrameBytes == 0 {
		t.Fatalf("status = %+v, %v", status, err)
	}
	health, err := client.Health(t.Context())
	if err != nil || !health.Healthy || health.FailingSubsystems == nil || health.SafeActions == nil {
		t.Fatalf("health = %+v, %v", health, err)
	}
	stats, err := client.Stats(t.Context())
	if err != nil || stats.SchemaVersion != 1 || stats.LiveRecords != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	config, err := client.DescribeConfig(t.Context())
	if err != nil || config.Source != "flag" || config.Effective.Limits.MaxFrameBytes == 0 || config.SafeCeilings.MaxFrameBytes != gapdb.HardMaxFrameBytes {
		t.Fatalf("config = %+v, %v", config, err)
	}
	verified, err := client.Verify(t.Context(), "full")
	if err != nil || !verified.Verified || len(verified.CheckedFiles) != 4 {
		t.Fatalf("verify = %+v, %v", verified, err)
	}
	snapshot, err := client.CreateSnapshot(t.Context(), status.DatabaseID, status.CurrentRevision)
	if err != nil || snapshot.Filename == "" || snapshot.Checksum == "" || snapshot.NewWALStart != snapshot.Revision+1 {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	compact, err := client.Compact(t.Context(), status.DatabaseID, status.CurrentRevision)
	if err != nil || !compact.DirectorySynced || compact.Removed == nil {
		t.Fatalf("compact = %+v, %v", compact, err)
	}
	backup, err := client.Backup(t.Context(), status.DatabaseID, status.CurrentRevision, filepath.Join(t.TempDir(), "backup"))
	if err != nil || backup.DatabaseID != status.DatabaseID || backup.Revision != status.CurrentRevision || backup.ManifestChecksum == "" || backup.ByteCount == 0 || !backup.Verified || len(backup.Files) == 0 {
		t.Fatalf("backup = %+v, %v", backup, err)
	}
}

func TestReviewerExistingLockIsExactOwnerOnlyInodeBeforeReadiness(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath, 0o666); err != nil {
		t.Fatal(err)
	}
	srv, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close(t.Context())
	info, err := os.Stat(lockPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("LOCK mode = %v, %v", info, err)
	}
}

func TestReviewerLockSymlinkAndIdentitySwapFailBeforeSocket(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, nil, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "LOCK")); err != nil {
			t.Fatal(err)
		}
		if _, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"}); err == nil {
			t.Fatal("symlink LOCK accepted")
		}
		if _, err := os.Stat(filepath.Join(dir, "gapdb.sock")); !os.IsNotExist(err) {
			t.Fatalf("socket created: %v", err)
		}
	})
	t.Run("swap", func(t *testing.T) {
		dir := t.TempDir()
		lockPath := filepath.Join(dir, "LOCK")
		if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test", BeforeLockModeCheck: func() {
			called = true
			_ = os.Rename(lockPath, lockPath+".held")
			_ = os.WriteFile(lockPath, nil, 0o666)
		}})
		if err == nil || !called {
			t.Fatalf("identity swap accepted: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "gapdb.sock")); !os.IsNotExist(statErr) {
			t.Fatalf("socket created: %v", statErr)
		}
	})
}

func TestReviewerLockedReplacementDoesNotImpersonateHeldLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var replacement *os.File
	_, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test", BeforeLockModeCheck: func() {
		if renameErr := os.Rename(lockPath, lockPath+".held"); renameErr != nil {
			t.Errorf("rename held LOCK: %v", renameErr)
			return
		}
		var openErr error
		replacement, openErr = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
		if openErr != nil {
			t.Errorf("create replacement LOCK: %v", openErr)
			return
		}
		if flockErr := unix.Flock(int(replacement.Fd()), unix.LOCK_EX|unix.LOCK_NB); flockErr != nil {
			t.Errorf("flock replacement LOCK: %v", flockErr)
		}
	}})
	if replacement != nil {
		_ = unix.Flock(int(replacement.Fd()), unix.LOCK_UN)
		_ = replacement.Close()
	}
	if err == nil {
		t.Fatal("locked replacement inode impersonated the runtime-held LOCK")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "gapdb.sock")); !os.IsNotExist(statErr) {
		t.Fatalf("socket created after LOCK substitution: %v", statErr)
	}
}

func TestReviewerDatabaseDirectorySwapFailsBeforeReadiness(t *testing.T) {
	for _, stage := range []string{"before_first_check", "after_stale_cleanup", "after_listener_bind"} {
		t.Run(stage, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "database")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			held := dir + ".held"
			var replacementLock *os.File
			var replacementListener *net.UnixListener
			var swapErr error
			swap := func() {
				if swapErr = os.Rename(dir, held); swapErr != nil {
					return
				}
				if swapErr = os.Mkdir(dir, 0o700); swapErr != nil {
					return
				}
				if swapErr = os.WriteFile(filepath.Join(dir, "sentinel"), []byte("replacement"), 0o600); swapErr != nil {
					return
				}
				if replacementLock, swapErr = os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o640); swapErr != nil {
					return
				}
				if swapErr = unix.Flock(int(replacementLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); swapErr != nil {
					return
				}
				replacementListener, swapErr = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "gapdb.sock"), Net: "unix"})
				if replacementListener != nil {
					replacementListener.SetUnlinkOnClose(false)
				}
			}
			config := server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"}
			switch stage {
			case "before_first_check":
				config.BeforeLockModeCheck = swap
			case "after_stale_cleanup":
				config.AfterStaleSocketCleanup = swap
			case "after_listener_bind":
				config.AfterListenerBind = swap
			}
			srv, err := server.Open(config)
			if replacementListener != nil {
				defer replacementListener.Close()
			}
			if replacementLock != nil {
				defer replacementLock.Close()
			}
			if swapErr != nil {
				t.Fatalf("directory replacement: %v", swapErr)
			}
			if srv != nil {
				_ = srv.Close(context.Background())
			}
			if err == nil {
				t.Fatal("replacement database directory received readiness")
			}
			value, readErr := os.ReadFile(filepath.Join(dir, "sentinel"))
			if readErr != nil || string(value) != "replacement" {
				t.Fatalf("replacement directory mutated: %q, %v", value, readErr)
			}
			lockInfo, statErr := os.Stat(filepath.Join(dir, "LOCK"))
			if statErr != nil || lockInfo.Mode().Perm() != 0o640 {
				t.Fatalf("replacement LOCK mutated: %v, %v", lockInfo, statErr)
			}
			if socketInfo, statErr := os.Lstat(filepath.Join(dir, "gapdb.sock")); statErr != nil || socketInfo.Mode()&os.ModeSocket == 0 {
				t.Fatalf("replacement socket mutated: %v, %v", socketInfo, statErr)
			}
			if _, statErr := os.Stat(filepath.Join(held, "gapdb.sock")); !os.IsNotExist(statErr) {
				t.Fatalf("anchored socket leaked after failed readiness: %v", statErr)
			}
		})
	}
}

func TestReviewerSeparateSocketParentSwapBeforeFirstCheckFailsClosed(t *testing.T) {
	for _, stage := range []string{"before_first_check", "after_stale_cleanup", "after_listener_bind"} {
		t.Run(stage, func(t *testing.T) {
			databaseDir := t.TempDir()
			parent := t.TempDir()
			socketDir := filepath.Join(parent, "socket")
			if err := os.Mkdir(socketDir, 0o700); err != nil {
				t.Fatal(err)
			}
			held := socketDir + ".held"
			socketPath := filepath.Join(socketDir, "gapdb.sock")
			var replacementListener *net.UnixListener
			var swapErr error
			swap := func() {
				if swapErr = os.Rename(socketDir, held); swapErr != nil {
					return
				}
				if swapErr = os.Mkdir(socketDir, 0o700); swapErr != nil {
					return
				}
				if swapErr = os.WriteFile(filepath.Join(socketDir, "sentinel"), []byte("replacement"), 0o600); swapErr != nil {
					return
				}
				replacementListener, swapErr = net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
				if replacementListener != nil {
					replacementListener.SetUnlinkOnClose(false)
				}
			}
			config := server.Config{Directory: databaseDir, SocketPath: socketPath, Options: gapdb.DefaultOptions(), ToolVersion: "test"}
			switch stage {
			case "before_first_check":
				config.BeforeLockModeCheck = swap
			case "after_stale_cleanup":
				config.AfterStaleSocketCleanup = swap
			case "after_listener_bind":
				config.AfterListenerBind = swap
			}
			srv, err := server.Open(config)
			if replacementListener != nil {
				defer replacementListener.Close()
			}
			if swapErr != nil {
				t.Fatalf("socket parent replacement: %v", swapErr)
			}
			if srv != nil {
				_ = srv.Close(context.Background())
			}
			if err == nil {
				t.Fatal("replacement socket parent received readiness")
			}
			value, readErr := os.ReadFile(filepath.Join(socketDir, "sentinel"))
			if readErr != nil || string(value) != "replacement" {
				t.Fatalf("replacement socket parent mutated: %q, %v", value, readErr)
			}
			if socketInfo, statErr := os.Lstat(socketPath); statErr != nil || socketInfo.Mode()&os.ModeSocket == 0 {
				t.Fatalf("replacement socket mutated: %v, %v", socketInfo, statErr)
			}
			if _, statErr := os.Stat(filepath.Join(held, "gapdb.sock")); !os.IsNotExist(statErr) {
				t.Fatalf("anchored socket leaked after failed readiness: %v", statErr)
			}
		})
	}
}

func TestCustomSocketPathDialCloseAndRestart(t *testing.T) {
	databaseDir := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "socket")
	if err := os.Mkdir(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "custom.sock")
	config := server.Config{Directory: databaseDir, SocketPath: socketPath, Options: gapdb.DefaultOptions(), ToolVersion: "test"}

	for attempt := range 2 {
		srv, err := server.Open(config)
		if err != nil {
			t.Fatalf("open attempt %d: %v", attempt, err)
		}
		client, err := gapdb.Dial(socketPath, gapdb.ClientOptions{})
		if err != nil {
			_ = srv.Close(context.Background())
			t.Fatalf("dial attempt %d: %v", attempt, err)
		}
		status, err := client.Status(t.Context())
		if err != nil || status.DatabaseID == "" {
			_ = client.Close()
			_ = srv.Close(context.Background())
			t.Fatalf("status attempt %d: %+v, %v", attempt, status, err)
		}
		if err := client.Close(); err != nil {
			t.Fatalf("client close attempt %d: %v", attempt, err)
		}
		if err := srv.Close(t.Context()); err != nil {
			t.Fatalf("server close attempt %d: %v", attempt, err)
		}
		if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
			t.Fatalf("socket remains after close attempt %d: %v", attempt, err)
		}
	}
}

func TestReviewerWatchRegistrationErrorRemainsStructured(t *testing.T) {
	options := gapdb.DefaultOptions()
	options.Limits.WatchBufferEvents = 1
	options.Limits.MaxWatchClients = 1
	options.Limits.MaxHistoryEvents = 1
	srv, err := server.Open(server.Config{Directory: t.TempDir(), Options: options, ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	watch, err := client.Watch(t.Context(), "", 1)
	if watch != nil {
		watch.Close()
		t.Fatal("ahead watch unexpectedly registered")
	}
	var transport *gapdb.TransportError
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionAhead}) || errors.As(err, &transport) {
		t.Fatalf("Watch error = %#v, want structured REVISION_AHEAD", err)
	}
	if _, err := client.Put(t.Context(), "watch/one", []byte("one"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Put(t.Context(), "watch/two", []byte("two"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if watch, err := client.Watch(t.Context(), "", 0); watch != nil || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionCompacted}) || errors.As(err, &transport) {
		if watch != nil {
			watch.Close()
		}
		t.Fatalf("compacted Watch = %v, %#v", watch, err)
	}
	active, err := client.Watch(t.Context(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if watch, err := client.Watch(t.Context(), "", 2); watch != nil || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeServerBusy}) || errors.As(err, &transport) {
		if watch != nil {
			watch.Close()
		}
		t.Fatalf("busy Watch = %v, %#v", watch, err)
	}
}

func TestMalformedOversizedFramesAndClientAdmissionAreBounded(t *testing.T) {
	dir := t.TempDir()
	options := gapdb.DefaultOptions()
	options.Limits.MaxConcurrentClients = 1
	options.Limits.MaxWatchClients = 1
	srv, err := server.Open(server.Config{Directory: dir, Options: options, ToolVersion: "test", ReadTimeout: 100 * time.Millisecond, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })

	first, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	time.Sleep(10 * time.Millisecond)
	second, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.ReadFrame(second, options.Limits.MaxFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeResponse(payload, options.Limits.MaxFrameBytes)
	if err != nil || response.Error == nil || response.Error.Code != gapdb.CodeServerBusy {
		t.Fatalf("admission response = %+v, %v", response, err)
	}
	_ = second.Close()
	_ = first.Close()

	for attempt := 0; attempt < 100; attempt++ {
		oversized, dialErr := net.Dial("unix", srv.SocketPath())
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(options.Limits.MaxFrameBytes+1))
		if _, err := oversized.Write(prefix[:]); err != nil {
			t.Fatal(err)
		}
		payload, err = protocol.ReadFrame(oversized, options.Limits.MaxFrameBytes)
		_ = oversized.Close()
		if err != nil {
			t.Fatal(err)
		}
		response, err = protocol.DecodeResponse(payload, options.Limits.MaxFrameBytes)
		if err != nil {
			t.Fatal(err)
		}
		if response.Error != nil && response.Error.Code == gapdb.CodeServerBusy {
			time.Sleep(time.Millisecond)
			continue
		}
		if response.Error == nil || response.Error.Code != gapdb.CodeFrameTooLarge {
			t.Fatalf("oversized response = %+v", response)
		}
		return
	}
	t.Fatal("client slot was not released")
}

func TestNonSocketStalePathIsNeverRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gapdb.sock")
	if err := os.WriteFile(path, []byte("authority-unknown"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodePermissionDenied}) {
		t.Fatalf("Open = %v", err)
	}
	value, err := os.ReadFile(path)
	if err != nil || string(value) != "authority-unknown" {
		t.Fatalf("stale non-socket mutated: %q, %v", value, err)
	}
}

func TestStaleUnixSocketIsRemovedOnlyAfterOwnershipAndUnlinkedOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gapdb.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remains after close: %v", err)
	}
}

func TestMissingIdentityNeverOverwritesOrphanedAuthority(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "CURRENT")
	if err := os.WriteFile(current, []byte("orphaned-authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRecoveryRequired}) {
		t.Fatalf("Open = %v", err)
	}
	value, err := os.ReadFile(current)
	if err != nil || string(value) != "orphaned-authority" {
		t.Fatalf("authority overwritten: %q, %v", value, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gapdb.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket created on recovery-required startup: %v", err)
	}
}

func TestDurableBarrierFailureRoundTripsAppliedEvidence(t *testing.T) {
	injected := errors.New("injected sync failure")
	fsys := faultfs.NewOS(faultfs.NewInjector(61, faultfs.Rule{Point: faultfs.PointWALFileSync, Phase: faultfs.Before, Occurrence: 2, Seed: 61, Err: injected}))
	srv, err := server.Open(server.Config{Directory: t.TempDir(), Options: gapdb.DefaultOptions(), ToolVersion: "test", FS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckDurable)
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeStorageDegraded || !structured.OperationApplied {
		t.Fatalf("Put error = %#v", err)
	}
	if _, err := client.Get(t.Context(), "key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("failed durable value was published: %v", err)
	}
}

func TestWatchLagRoundTripsLastFullyWrittenRevision(t *testing.T) {
	options := gapdb.DefaultOptions()
	options.Limits.WatchBufferEvents = 2
	srv, err := server.Open(server.Config{Directory: t.TempDir(), Options: options, ToolVersion: "test", WriteTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	watch, err := client.Watch(t.Context(), "lag/", 0)
	if err != nil {
		t.Fatal(err)
	}
	value := make([]byte, 1<<20)
	for index := 0; index < 8; index++ {
		if _, err := client.Put(t.Context(), fmt.Sprintf("lag/%02d", index), value, nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	var last gapdb.Revision
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event, ok := <-watch.Events:
			if ok {
				last = event.Revision
			}
		case termination := <-watch.Ended:
			if termination.Error == nil || termination.Error.Code != gapdb.CodeWatchLagged || termination.LastDeliveredRevision != last {
				t.Fatalf("termination=%+v cause=%#v last=%d", termination, termination.Error.Cause, last)
			}
			return
		case <-deadline:
			t.Fatal("lag termination timed out")
		}
	}
}

func TestWatchStreamsAndCancels(t *testing.T) {
	dir := t.TempDir()
	srv, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	watch, err := client.Watch(ctx, "p/", 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Put(t.Context(), "p/a", []byte("v"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-watch.Events:
		if event.Revision != result.Revision || event.Key != "p/a" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("watch event timed out")
	}
	cancel()
	select {
	case termination := <-watch.Ended:
		if termination.Reason != gapdb.WatchEndedByClient {
			t.Fatalf("termination = %+v", termination)
		}
	case <-time.After(time.Second):
		t.Fatal("watch cancellation timed out")
	}
}

func TestShutdownTerminatesWatchBeforeUnlinkAndReleasesOwner(t *testing.T) {
	dir := t.TempDir()
	srv, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test", WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	watch, err := client.Watch(t.Context(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case termination := <-watch.Ended:
		if termination.Reason != gapdb.WatchEndedByShutdown {
			t.Fatalf("termination = %+v", termination)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown watch termination timed out")
	}
	reopened, err := server.Open(server.Config{Directory: dir, Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatalf("owner was not released: %v", err)
	}
	_ = reopened.Close(t.Context())
}
