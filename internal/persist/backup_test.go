package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

func TestCreateVerifyAndRestoreBackup(t *testing.T) {
	source := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	identity := Identity{DatabaseID: id, ReservedRevisionEnd: 10, Generation: 1}
	identityBytes, _ := EncodeIdentity(identity)
	if err := os.WriteFile(filepath.Join(source, IdentityFilename), identityBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	state := SnapshotState{Revision: 2, Records: []gapdb.Record{gapdb.NewRecord("k", []byte{0, 0xff}, 2, nil)}}
	snapshot, digest, err := EncodeSnapshot(id, state, time.Unix(0, 0), gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{DatabaseID: id, Generation: 2, SnapshotFile: "snapshot-00000000000000000002.gdb", SnapshotRevision: 2, SnapshotSHA256: fmtDigest(digest), WALFile: "wal-00000000000000000003.gdb", WALStartRevision: 3}
	if err := os.WriteFile(filepath.Join(source, manifest.SnapshotFile), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	wal, err := CreateWAL(faultfs.NewOS(nil), filepath.Join(source, manifest.WALFile), WALHeader{DatabaseID: id, FirstRevision: 3, Generation: 2}, 128, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Append(CommitFrame{Revision: 3, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "x", Value: []byte("y")}}}); err != nil {
		t.Fatal(err)
	}
	if err := wal.Barrier(); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	writeManifestDirect(t, source, manifest)
	destination := filepath.Join(filepath.Dir(source), "backup-final")
	metadata, err := CreateBackup(BackupOptions{FS: faultfs.NewOS(nil), SourceDirectory: source, Destination: destination, ExpectedDatabaseID: id, ExpectedDurableRevision: 3, CreatedAt: time.Unix(123, 0).UTC(), BackupID: "backup-1", ToolVersion: "test", Limits: gapdb.DefaultOptions().Limits, Barrier: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Files) != 4 {
		t.Fatalf("metadata = %+v", metadata)
	}
	verified, err := VerifyBackup(faultfs.NewOS(nil), destination, gapdb.DefaultOptions().Limits)
	if err != nil || verified.DurableRevision != 3 {
		t.Fatalf("verify = %+v, %v", verified, err)
	}
	for _, file := range append(metadata.Files, BackupFile{Name: BackupMetadataFilename}) {
		path := filepath.Join(destination, file.Name)
		original, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		mutated := append([]byte(nil), original...)
		mutated[0] ^= 1
		if err := os.WriteFile(path, mutated, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, verifyErr := VerifyBackup(faultfs.NewOS(nil), destination, gapdb.DefaultOptions().Limits); !errors.Is(verifyErr, &gapdb.Error{Code: gapdb.CodeBackupInvalid}) {
			t.Fatalf("tampered %s = %v", file.Name, verifyErr)
		}
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(destination, "audit.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(faultfs.NewOS(nil), destination, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBackupInvalid}) {
		t.Fatalf("extra file verify = %v", err)
	}
	if err := os.Remove(filepath.Join(destination, "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	restore := filepath.Join(filepath.Dir(source), "restore-final")
	if _, err := RestoreBackup(faultfs.NewOS(nil), destination, restore, gapdb.DefaultOptions().Limits); err != nil {
		t.Fatal(err)
	}
	got, err := ReadIdentity(faultfs.NewOS(nil), restore)
	if err != nil || got.DatabaseID != id {
		t.Fatalf("restored identity = %+v, %v", got, err)
	}
}

func TestBackupRejectsUnsafeDestinationsAndTampering(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateBackup(BackupOptions{FS: faultfs.NewOS(nil), SourceDirectory: dir, Destination: filepath.Join(dir, "inside")}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) {
		t.Fatalf("inside = %v", err)
	}
	existing := filepath.Join(filepath.Dir(dir), "already-there")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateBackup(BackupOptions{FS: faultfs.NewOS(nil), SourceDirectory: dir, Destination: existing}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBackupDestinationExists}) {
		t.Fatalf("existing = %v", err)
	}
}

func TestBackupFaultsNeverPublishAnUnverifiedDestination(t *testing.T) {
	for _, point := range []faultfs.Point{faultfs.PointBackupFileCopy, faultfs.PointBackupFileSync, faultfs.PointBackupRename} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"-"+string(phase), func(t *testing.T) {
				source, id := makeBackupSource(t)
				destination := filepath.Join(filepath.Dir(source), "fault-backup")
				injector := faultfs.NewInjector(23, faultfs.Rule{Point: point, Phase: phase, Occurrence: 1, Seed: 23, Err: os.ErrPermission})
				_, err := CreateBackup(BackupOptions{FS: faultfs.NewOS(injector), SourceDirectory: source, Destination: destination, ExpectedDatabaseID: id, ExpectedDurableRevision: 3, CreatedAt: time.Unix(123, 0).UTC(), BackupID: "fault", ToolVersion: "test", Limits: gapdb.DefaultOptions().Limits, Barrier: func() error { return nil }})
				if err == nil {
					t.Fatal("fault unexpectedly succeeded")
				}
				if point == faultfs.PointBackupRename && phase == faultfs.After {
					if _, verifyErr := VerifyBackup(faultfs.NewOS(nil), destination, gapdb.DefaultOptions().Limits); verifyErr != nil {
						t.Fatalf("published backup incomplete: %v", verifyErr)
					}
				} else if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("final destination published before rename: %v", statErr)
				}
			})
		}
	}
}

func TestBackupAndRestoreRejectSymlinkDestinationsAndRenameRaces(t *testing.T) {
	t.Run("backup-parent-alias-inside-source", func(t *testing.T) {
		source, id := makeBackupSource(t)
		inside := filepath.Join(source, "inside")
		if err := os.Mkdir(inside, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(inside, alias); err != nil {
			t.Fatal(err)
		}
		_, err := CreateBackup(backupOptionsForTest(source, filepath.Join(alias, "backup"), id))
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) {
			t.Fatalf("alias backup=%v", err)
		}
	})
	t.Run("backup-dangling-final", func(t *testing.T) {
		source, id := makeBackupSource(t)
		destination := filepath.Join(t.TempDir(), "backup")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), destination); err != nil {
			t.Fatal(err)
		}
		_, err := CreateBackup(backupOptionsForTest(source, destination, id))
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBackupDestinationExists}) {
			t.Fatalf("dangling backup=%v", err)
		}
	})
	t.Run("backup-rename-race", func(t *testing.T) {
		source, id := makeBackupSource(t)
		destination := filepath.Join(t.TempDir(), "backup")
		injector := faultfs.NewInjector(41, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.Before, Occurrence: 1, Seed: 41, Observe: func(faultfs.Event) {
			if err := os.Mkdir(destination, 0o700); err != nil {
				panic(err)
			}
		}})
		options := backupOptionsForTest(source, destination, id)
		options.FS = faultfs.NewOS(injector)
		_, err := CreateBackup(options)
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBackupDestinationExists}) {
			t.Fatalf("race backup=%v", err)
		}
		if info, statErr := os.Stat(destination); statErr != nil || !info.IsDir() {
			t.Fatalf("raced destination lost: %v", statErr)
		}
	})

	backupSource, id := makeBackupSource(t)
	backup := filepath.Join(filepath.Dir(backupSource), "valid-backup")
	if _, err := CreateBackup(backupOptionsForTest(backupSource, backup, id)); err != nil {
		t.Fatal(err)
	}
	t.Run("restore-parent-alias-inside-backup", func(t *testing.T) {
		inside := filepath.Join(backup, "inside")
		if err := os.Mkdir(inside, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(inside, alias); err != nil {
			t.Fatal(err)
		}
		_, err := RestoreBackup(faultfs.NewOS(nil), backup, filepath.Join(alias, "restore"), gapdb.DefaultOptions().Limits)
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) {
			t.Fatalf("alias restore=%v", err)
		}
		if err := os.RemoveAll(inside); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("restore-dangling-final", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "restore")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), destination); err != nil {
			t.Fatal(err)
		}
		_, err := RestoreBackup(faultfs.NewOS(nil), backup, destination, gapdb.DefaultOptions().Limits)
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBackupDestinationExists}) {
			t.Fatalf("dangling restore=%v", err)
		}
	})
	t.Run("restore-rename-race", func(t *testing.T) {
		destination := filepath.Join(t.TempDir(), "restore")
		injector := faultfs.NewInjector(42, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.Before, Occurrence: 1, Seed: 42, Observe: func(faultfs.Event) {
			if err := os.Mkdir(destination, 0o700); err != nil {
				panic(err)
			}
		}})
		_, err := RestoreBackup(faultfs.NewOS(injector), backup, destination, gapdb.DefaultOptions().Limits)
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBackupDestinationExists}) {
			t.Fatalf("race restore=%v", err)
		}
	})
}

func TestBackupAndRestoreAnchorFinalPublicationIdentities(t *testing.T) {
	for _, operation := range []string{"backup", "restore"} {
		for _, attack := range []string{"source-swap", "parent-swap"} {
			t.Run(operation+"-"+attack, func(t *testing.T) {
				source, id := makeBackupSource(t)
				backup := filepath.Join(filepath.Dir(source), "seed-backup")
				if operation == "restore" {
					if _, err := CreateBackup(backupOptionsForTest(source, backup, id)); err != nil {
						t.Fatal(err)
					}
				}
				root := t.TempDir()
				parent := filepath.Join(root, "parent")
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				destination := filepath.Join(parent, "published")
				movedParent := parent + "-moved"
				injector := faultfs.NewInjector(93, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.Before, Occurrence: 1, Seed: 93, Observe: func(faultfs.Event) {
					if attack == "parent-swap" {
						if err := os.Rename(parent, movedParent); err != nil {
							panic(err)
						}
						inside := filepath.Join(source, "redirect")
						if err := os.Mkdir(inside, 0o700); err != nil {
							panic(err)
						}
						if err := os.Symlink(inside, parent); err != nil {
							panic(err)
						}
						return
					}
					entries, err := os.ReadDir(parent)
					if err != nil {
						panic(err)
					}
					for _, entry := range entries {
						if entry.IsDir() && strings.Contains(entry.Name(), ".tmp-") {
							temp := filepath.Join(parent, entry.Name())
							if err := os.Rename(temp, temp+"-inspected"); err != nil {
								panic(err)
							}
							if err := os.Mkdir(temp, 0o700); err != nil {
								panic(err)
							}
							return
						}
					}
					panic("publication temp not found")
				}})
				var err error
				if operation == "backup" {
					options := backupOptionsForTest(source, destination, id)
					options.FS = faultfs.NewOS(injector)
					_, err = CreateBackup(options)
				} else {
					_, err = RestoreBackup(faultfs.NewOS(injector), backup, destination, gapdb.DefaultOptions().Limits)
				}
				if err == nil {
					t.Fatal("identity swap published successfully")
				}
				if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("redirected or overwritten destination: %v", statErr)
				}
			})
		}
	}
}

func TestBackupAndRestorePinPublishedDirectoryThroughDurabilitySync(t *testing.T) {
	for _, operation := range []string{"backup", "restore"} {
		t.Run(operation, func(t *testing.T) {
			source, id := makeBackupSource(t)
			backup := filepath.Join(filepath.Dir(source), "seed-backup")
			if operation == "restore" {
				if _, err := CreateBackup(backupOptionsForTest(source, backup, id)); err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(parent, "published")
			moved := parent + "-moved"
			injector := faultfs.NewInjector(97, faultfs.Rule{Point: faultfs.PointPublicationDirectorySync, Phase: faultfs.Before, Occurrence: 1, Seed: 97, Observe: func(faultfs.Event) {
				if err := os.Rename(parent, moved); err != nil {
					panic(err)
				}
				if err := os.Mkdir(parent, 0o700); err != nil {
					panic(err)
				}
			}})
			var result BackupMetadata
			var err error
			if operation == "backup" {
				options := backupOptionsForTest(source, destination, id)
				options.FS = faultfs.NewOS(injector)
				result, err = CreateBackup(options)
			} else {
				result, err = RestoreBackup(faultfs.NewOS(injector), backup, destination, gapdb.DefaultOptions().Limits)
			}
			var structured *gapdb.Error
			actual := filepath.Join(moved, "published")
			if !errors.As(err, &structured) || !structured.OperationApplied || result.Destination != actual {
				t.Fatalf("result=%+v err=%#v", result, err)
			}
			if _, statErr := os.Stat(actual); statErr != nil {
				t.Fatalf("actual publication missing: %v", statErr)
			}
			if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("replacement directory received publication: %v", statErr)
			}
		})
	}
}

func TestBackupAndRestorePublicationSyncFaultsPreserveStableEvidence(t *testing.T) {
	for _, operation := range []string{"backup", "restore"} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(operation+"-"+string(phase), func(t *testing.T) {
				source, id := makeBackupSource(t)
				backup := filepath.Join(filepath.Dir(source), "seed-backup")
				if operation == "restore" {
					if _, err := CreateBackup(backupOptionsForTest(source, backup, id)); err != nil {
						t.Fatal(err)
					}
				}
				destination := filepath.Join(t.TempDir(), "published")
				injector := faultfs.NewInjector(98, faultfs.Rule{Point: faultfs.PointPublicationDirectorySync, Phase: phase, Occurrence: 1, Seed: 98, Err: os.ErrPermission})
				var result BackupMetadata
				var err error
				if operation == "backup" {
					options := backupOptionsForTest(source, destination, id)
					options.FS = faultfs.NewOS(injector)
					result, err = CreateBackup(options)
				} else {
					result, err = RestoreBackup(faultfs.NewOS(injector), backup, destination, gapdb.DefaultOptions().Limits)
				}
				var structured *gapdb.Error
				if !errors.As(err, &structured) || !structured.OperationApplied || result.Destination != destination || result.DurableRevision != 3 {
					t.Fatalf("result=%+v err=%#v", result, err)
				}
				if _, statErr := os.Stat(destination); statErr != nil {
					t.Fatalf("published result absent: %v", statErr)
				}
			})
		}
	}
}

func backupOptionsForTest(source, destination string, id DatabaseID) BackupOptions {
	return BackupOptions{FS: faultfs.NewOS(nil), SourceDirectory: source, Destination: destination, ExpectedDatabaseID: id, ExpectedDurableRevision: 3, CreatedAt: time.Unix(123, 0).UTC(), BackupID: "paths", ToolVersion: "test", Limits: gapdb.DefaultOptions().Limits, Barrier: func() error { return nil }}
}

func makeBackupSource(t *testing.T) (string, DatabaseID) {
	t.Helper()
	source := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	identityBytes, _ := EncodeIdentity(Identity{DatabaseID: id, ReservedRevisionEnd: 10, Generation: 1})
	if err := os.WriteFile(filepath.Join(source, IdentityFilename), identityBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, digest, err := EncodeSnapshot(id, SnapshotState{Revision: 2}, time.Unix(0, 0), gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{DatabaseID: id, Generation: 2, SnapshotFile: "snapshot-00000000000000000002.gdb", SnapshotRevision: 2, SnapshotSHA256: fmtDigest(digest), WALFile: "wal-00000000000000000003.gdb", WALStartRevision: 3}
	if err := os.WriteFile(filepath.Join(source, manifest.SnapshotFile), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	wal, err := CreateWAL(faultfs.NewOS(nil), filepath.Join(source, manifest.WALFile), WALHeader{DatabaseID: id, FirstRevision: 3, Generation: 2}, 128, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Append(CommitFrame{Revision: 3, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "x", Value: []byte("y")}}}); err != nil {
		t.Fatal(err)
	}
	if err := wal.Barrier(); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	writeManifestDirect(t, source, manifest)
	return source, id
}

func fmtDigest(value [32]byte) string { return fmt.Sprintf("%x", value[:]) }
