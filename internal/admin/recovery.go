package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
	"gapdb/internal/persist"
)

type RecoveryProposal struct {
	ID                 string             `json:"id"`
	DatabaseID         string             `json:"database_id"`
	ManifestGeneration uint64             `json:"manifest_generation"`
	Action             string             `json:"action"`
	FindingCode        gapdb.ErrorCode    `json:"finding_code"`
	AllowedActions     []gapdb.SafeAction `json:"allowed_actions"`
	File               string             `json:"file"`
	EvidenceSHA256     string             `json:"evidence_sha256"`
	EvidenceSize       int64              `json:"evidence_size"`
	EvidenceDevice     uint64             `json:"evidence_device"`
	EvidenceInode      uint64             `json:"evidence_inode"`
}
type proposalEvidence struct {
	DatabaseID         string             `json:"database_id"`
	ManifestGeneration uint64             `json:"manifest_generation"`
	Action             string             `json:"action"`
	FindingCode        gapdb.ErrorCode    `json:"finding_code"`
	AllowedActions     []gapdb.SafeAction `json:"allowed_actions"`
	File               string             `json:"file"`
	EvidenceSHA256     string             `json:"evidence_sha256"`
	EvidenceSize       int64              `json:"evidence_size"`
	EvidenceDevice     uint64             `json:"evidence_device"`
	EvidenceInode      uint64             `json:"evidence_inode"`
}
type ApplyOptions struct {
	ExpectedDatabaseID         string
	ExpectedManifestGeneration uint64
	QuarantineDirectory        string
	RequestID                  string
	Audit                      AuditOptions
	Timestamp                  time.Time
}
type ApplyResult struct {
	ProposalID       string `json:"proposal_id"`
	QuarantinedPath  string `json:"quarantined_path"`
	OperationApplied bool   `json:"operation_applied"`
}

func ProposeQuarantine(report Inspection) (RecoveryProposal, error) {
	if report.DatabaseID == "" || report.ManifestGeneration == 0 || report.FindingCode == "" || filepath.Base(report.FindingFile) != report.FindingFile || report.FindingFile == "" || report.EvidenceSHA256 == "" || report.EvidenceSize < 0 || report.EvidenceDevice == 0 || report.EvidenceInode == 0 {
		return RecoveryProposal{}, recoveryMismatch("inspection evidence is incomplete")
	}
	actions, authorized := recoveryActions(report.FindingCode)
	if !authorized {
		return RecoveryProposal{}, recoveryMismatch("finding does not authorize recovery proposal")
	}
	evidence := proposalEvidence{report.DatabaseID, report.ManifestGeneration, "quarantine", report.FindingCode, actions, report.FindingFile, report.EvidenceSHA256, report.EvidenceSize, report.EvidenceDevice, report.EvidenceInode}
	encoded, _ := json.Marshal(evidence)
	digest := sha256.Sum256(encoded)
	return RecoveryProposal{ID: hex.EncodeToString(digest[:]), DatabaseID: evidence.DatabaseID, ManifestGeneration: evidence.ManifestGeneration, Action: evidence.Action, FindingCode: evidence.FindingCode, AllowedActions: append([]gapdb.SafeAction(nil), actions...), File: evidence.File, EvidenceSHA256: evidence.EvidenceSHA256, EvidenceSize: evidence.EvidenceSize, EvidenceDevice: evidence.EvidenceDevice, EvidenceInode: evidence.EvidenceInode}, nil
}

func ApplyRecovery(fsys faultfs.FS, directory string, proposal RecoveryProposal, options ApplyOptions) (ApplyResult, error) {
	if options.ExpectedDatabaseID != proposal.DatabaseID || options.ExpectedManifestGeneration != proposal.ManifestGeneration || proposal.Action != "quarantine" {
		return ApplyResult{}, adminPreconditionError("proposal preconditions do not match")
	}
	expected, _ := proposalFromFields(proposal)
	if expected.ID != proposal.ID {
		return ApplyResult{}, recoveryMismatch("proposal ID does not match its evidence")
	}
	canonicalActions, authorized := recoveryActions(proposal.FindingCode)
	if !authorized || !sameSafeActions(proposal.AllowedActions, canonicalActions) {
		return ApplyResult{}, recoveryMismatch("proposal finding does not authorize its action")
	}
	owner, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		return ApplyResult{}, err
	}
	defer owner.Close()
	current, inspectErr := inspectHeld(fsys, directory, gapdb.DefaultOptions().Limits)
	if inspectErr == nil {
		return ApplyResult{}, recoveryMismatch("current inspection finding does not match proposal")
	}
	currentActions, currentAuthorized := recoveryActions(current.FindingCode)
	if !currentAuthorized {
		return ApplyResult{}, inspectErr
	}
	if current.FindingCode != proposal.FindingCode || current.FindingFile != proposal.File || current.DatabaseID != proposal.DatabaseID || current.ManifestGeneration != proposal.ManifestGeneration || current.EvidenceSHA256 != proposal.EvidenceSHA256 || current.EvidenceSize != proposal.EvidenceSize || current.EvidenceDevice != proposal.EvidenceDevice || current.EvidenceInode != proposal.EvidenceInode || !sameSafeActions(proposal.AllowedActions, currentActions) {
		return ApplyResult{}, recoveryMismatch("current finding no longer authorizes recovery")
	}
	identity, err := persist.ReadIdentity(fsys, directory)
	if err != nil {
		return ApplyResult{}, err
	}
	if identity.DatabaseID.String() != proposal.DatabaseID {
		return ApplyResult{}, adminPreconditionError("database ID changed")
	}
	manifest, err := persist.ReadManifest(fsys, directory, identity.DatabaseID, 1)
	if err != nil {
		return ApplyResult{}, err
	}
	if manifest.Generation != proposal.ManifestGeneration {
		return ApplyResult{}, adminPreconditionError("manifest generation changed")
	}
	actual, evidenceErr := artifactEvidenceFor(fsys, directory, proposal.File)
	if evidenceErr != nil || !sameArtifactEvidence(proposal, actual) {
		return ApplyResult{}, recoveryMismatch("artifact evidence changed")
	}
	if options.QuarantineDirectory == "" {
		return ApplyResult{}, adminPreconditionError("quarantine directory is required")
	}
	if err := persist.ValidateExternalDirectory(directory, options.QuarantineDirectory); err != nil {
		return ApplyResult{}, adminPreconditionError(err.Error())
	}
	destination := filepath.Join(options.QuarantineDirectory, proposal.File)
	if err := persist.ValidateNewDestination(directory, destination); err != nil {
		return ApplyResult{}, adminPreconditionError(err.Error())
	}
	expectedIdentity := faultfs.FileIdentity{Device: proposal.EvidenceDevice, Inode: proposal.EvidenceInode}
	publication, err := faultfs.PublishNoReplaceAnchored(fsys, faultfs.PointBackupRename, faultfs.PointPublicationDirectorySync, directory, proposal.File, options.QuarantineDirectory, proposal.File, expectedIdentity)
	stablePath := filepath.Base(options.QuarantineDirectory) + "/" + proposal.File
	if publication.DestinationParent != "" {
		stablePath = filepath.Base(publication.DestinationParent) + "/" + proposal.File
	}
	if err != nil {
		if faultfs.FailedAfter(err) {
			return ApplyResult{ProposalID: proposal.ID, QuarantinedPath: stablePath, OperationApplied: true}, appliedIO("recovery_quarantine_rename", err)
		}
		if errors.Is(err, os.ErrExist) {
			return ApplyResult{}, adminPreconditionError("quarantine destination exists")
		}
		return ApplyResult{}, adminStorageError("recovery_quarantine_rename", err, false)
	}
	result := ApplyResult{ProposalID: proposal.ID, QuarantinedPath: stablePath, OperationApplied: true}
	if options.Timestamp.IsZero() {
		options.Timestamp = time.Now().UTC()
	}
	entry := AuditEntry{SchemaVersion: 1, EventID: proposal.ID, Timestamp: options.Timestamp, DatabaseID: identity.DatabaseID, Operation: "recover_apply_quarantine", RequestID: options.RequestID, Outcome: "applied", Paths: []string{proposal.File}, SafeActions: []gapdb.SafeAction{gapdb.ActionVerify}}
	if entry.RequestID == "" {
		entry.RequestID = proposal.ID
	}
	if err := AppendAuditAfterApply(fsys, directory, entry, options.Audit); err != nil {
		return result, err
	}
	return result, nil
}

func proposalFromFields(value RecoveryProposal) (RecoveryProposal, error) {
	e := proposalEvidence{value.DatabaseID, value.ManifestGeneration, value.Action, value.FindingCode, value.AllowedActions, value.File, value.EvidenceSHA256, value.EvidenceSize, value.EvidenceDevice, value.EvidenceInode}
	encoded, _ := json.Marshal(e)
	digest := sha256.Sum256(encoded)
	value.ID = hex.EncodeToString(digest[:])
	return value, nil
}

func recoveryActions(code gapdb.ErrorCode) ([]gapdb.SafeAction, bool) {
	for _, definition := range gapdb.ErrorDefinitions() {
		if definition.Code != code {
			continue
		}
		for _, action := range definition.SafeActions {
			if action == gapdb.ActionRecoverPropose {
				return append([]gapdb.SafeAction(nil), definition.SafeActions...), true
			}
		}
		return nil, false
	}
	return nil, false
}

func sameSafeActions(left, right []gapdb.SafeAction) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
func recoveryMismatch(reason string) error {
	return &gapdb.Error{Code: gapdb.CodeRecoveryActionMismatch, Message: "Recovery action no longer matches the evidence.", Retry: gapdb.RetryAfterReconcile, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionRecoverPropose, gapdb.ActionAbort}}
}
func adminPreconditionError(reason string) error {
	return &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed, Message: "Administrative preconditions did not match.", Retry: gapdb.RetryAfterReconcile, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionRebuildRequest, gapdb.ActionAbort}}
}
func appliedIO(stage string, cause error) error {
	return &gapdb.Error{Code: gapdb.CodeIOError, Message: "A recovery storage operation failed after applying the action.", Retry: gapdb.RetryAfterOperator, Operation: stage, OSErrorCategory: "other", OperationApplied: true, SafeActions: []gapdb.SafeAction{gapdb.ActionCheckStorage, gapdb.ActionVerify, gapdb.ActionAbort}, Cause: cause}
}
func adminStorageError(stage string, cause error, applied bool) error {
	return &gapdb.Error{Code: gapdb.CodeIOError, Message: "An administrative storage operation failed.", Retry: gapdb.RetryAfterOperator, Operation: stage, OSErrorCategory: "other", OperationApplied: applied, SafeActions: []gapdb.SafeAction{gapdb.ActionCheckStorage, gapdb.ActionVerify, gapdb.ActionAbort}, Cause: cause}
}
