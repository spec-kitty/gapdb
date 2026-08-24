package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
	"gapdb/internal/persist"
)

func InspectOffline(fsys faultfs.FS, directory string, limits gapdb.Limits) (Inspection, error) {
	owner, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		return Inspection{}, err
	}
	defer owner.Close()
	return inspectHeld(fsys, directory, limits)
}

// inspectHeld performs the authority reads after the caller has acquired the
// database owner lock. Recovery apply uses this to bind a proposal to the
// current finding without releasing ownership between inspection and rename.
func inspectHeld(fsys faultfs.FS, directory string, limits gapdb.Limits) (Inspection, error) {
	report := Inspection{SchemaVersion: 1}
	identity, err := persist.ReadIdentity(fsys, directory)
	if err != nil {
		report.setEvidence(artifactEvidenceFor(fsys, directory, findingFile(err)))
		return inspectionError(report, err)
	}
	report.DatabaseID = identity.DatabaseID.String()
	manifest, err := persist.ReadManifest(fsys, directory, identity.DatabaseID, 1)
	if err != nil {
		report.setEvidence(artifactEvidenceFor(fsys, directory, findingFile(err)))
		return inspectionError(report, err)
	}
	report.ManifestGeneration = manifest.Generation
	report.SnapshotRevision = manifest.SnapshotRevision
	_, _, revision, err := persist.VerifyGeneration(fsys, directory, identity.DatabaseID, limits)
	if err != nil {
		report.setEvidence(artifactEvidenceFor(fsys, directory, findingFile(err)))
		return inspectionError(report, err)
	}
	report.CurrentRevision = revision
	report.Verified = true
	return report, nil
}
func VerifyOffline(fsys faultfs.FS, directory string, limits gapdb.Limits) (Inspection, error) {
	return InspectOffline(fsys, directory, limits)
}
func findingFile(err error) string {
	var structured *gapdb.Error
	if errors.As(err, &structured) {
		if structured.File != "" {
			return structured.File
		}
		return structured.Path
	}
	return ""
}

type artifactEvidence struct {
	SHA256 string
	Size   int64
	Device uint64
	Inode  uint64
}

func (report *Inspection) setEvidence(evidence artifactEvidence, err error) {
	if err != nil {
		return
	}
	report.EvidenceSHA256 = evidence.SHA256
	report.EvidenceSize = evidence.Size
	report.EvidenceDevice = evidence.Device
	report.EvidenceInode = evidence.Inode
}

func artifactEvidenceFor(fsys faultfs.FS, directory, name string) (artifactEvidence, error) {
	if filepath.Base(name) != name || name == "" {
		return artifactEvidence{}, os.ErrInvalid
	}
	file, err := fsys.OpenFile(faultfs.PointOpen, filepath.Join(directory, name), os.O_RDONLY, 0)
	if err != nil {
		return artifactEvidence{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return artifactEvidence{}, os.ErrInvalid
	}
	hash := sha256.New()
	written, err := io.CopyBuffer(hash, file, make([]byte, 64<<10))
	if err != nil {
		return artifactEvidence{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return artifactEvidence{}, err
	}
	if written != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return artifactEvidence{}, errors.New("artifact changed while hashing")
	}
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if !beforeOK || !afterOK || beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino {
		return artifactEvidence{}, errors.New("artifact identity changed while hashing")
	}
	return artifactEvidence{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: written, Device: uint64(beforeStat.Dev), Inode: uint64(beforeStat.Ino)}, nil
}

func sameArtifactEvidence(report RecoveryProposal, actual artifactEvidence) bool {
	return report.EvidenceSHA256 == actual.SHA256 && report.EvidenceSize == actual.Size && report.EvidenceDevice == actual.Device && report.EvidenceInode == actual.Inode
}
