package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/admin"
	"gapdb/internal/faultfs"
	"gapdb/internal/protocol"
)

const maxProposalBytes = 64 << 10

func runOfflineCommand(options globalOptions, command string, arguments []string, stdin io.Reader, stdout io.Writer) int {
	operation := offlineOperation(command)
	if options.socket != "" {
		return writeError(stdout, operation, options.requestID, invalidRequest("target", "--socket cannot be combined with an offline command"))
	}
	if options.database == "" || !filepath.IsAbs(options.database) || filepath.Clean(options.database) != options.database {
		return writeError(stdout, operation, options.requestID, invalidRequest("db", "offline commands require a clean absolute database path"))
	}
	fsys := faultfs.NewOS(nil)
	switch command {
	case "inspect":
		if len(arguments) != 0 {
			return writeError(stdout, operation, options.requestID, invalidRequest("arguments", "inspect takes no arguments"))
		}
		report, err := admin.InspectOffline(fsys, options.database, gapdb.DefaultOptions().Limits)
		if err != nil {
			return writeErrorEvidence(stdout, operation, options.requestID, err, inspectionEvidence(report))
		}
		return writeSuccess(stdout, operation, options.requestID, report)
	case "verify":
		flags := commandFlags(command)
		mode := flags.String("mode", "full", "sampled or full")
		if err := parseCommandFlags(flags, arguments); err != nil {
			return writeError(stdout, operation, options.requestID, err)
		}
		if *mode != "sampled" && *mode != "full" {
			return writeError(stdout, operation, options.requestID, invalidRequest("mode", "must be sampled or full"))
		}
		report, err := admin.VerifyOffline(fsys, options.database, gapdb.DefaultOptions().Limits)
		if err != nil {
			return writeErrorEvidence(stdout, operation, options.requestID, err, inspectionEvidence(report))
		}
		return writeSuccess(stdout, operation, options.requestID, struct {
			Mode       string           `json:"mode"`
			Inspection admin.Inspection `json:"inspection"`
		}{*mode, report})
	case "recover-propose":
		if len(arguments) != 0 {
			return writeError(stdout, operation, options.requestID, invalidRequest("arguments", "recover-propose takes no arguments"))
		}
		report, inspectErr := admin.InspectOffline(fsys, options.database, gapdb.DefaultOptions().Limits)
		if inspectErr == nil {
			return writeErrorEvidence(stdout, operation, options.requestID, invalidRequest("database", "verified storage has no recovery proposal"), inspectionEvidence(report))
		}
		proposal, err := admin.ProposeQuarantine(report)
		if err != nil {
			return writeErrorEvidence(stdout, operation, options.requestID, inspectErr, inspectionEvidence(report))
		}
		return writeSuccess(stdout, operation, options.requestID, struct {
			Inspection admin.Inspection       `json:"inspection"`
			Proposal   admin.RecoveryProposal `json:"proposal"`
		}{report, proposal})
	case "recover-apply":
		return runRecoveryApply(options, arguments, stdin, stdout, fsys)
	default:
		return writeError(stdout, operation, options.requestID, invalidRequest("command", "unknown offline command"))
	}
}

func offlineOperation(command string) string {
	switch command {
	case "inspect":
		return "offline_inspect"
	case "verify":
		return "offline_verify"
	case "recover-propose":
		return "offline_recover_propose"
	case "recover-apply":
		return "offline_recover_apply"
	default:
		return command
	}
}

func runRecoveryApply(options globalOptions, arguments []string, stdin io.Reader, stdout io.Writer, fsys faultfs.FS) int {
	const operation = "offline_recover_apply"
	flags := commandFlags("recover-apply")
	var proposalFile optionalString
	flags.Var(&proposalFile, "proposal-file", "strict recovery proposal JSON file")
	proposalStdin := flags.Bool("proposal-stdin", false, "read strict recovery proposal JSON from stdin")
	actionID := flags.String("action-id", "", "exact proposal action ID")
	databaseID := flags.String("expected-database-id", "", "exact database ID")
	generationText := flags.String("expected-manifest-generation", "", "exact manifest generation")
	destination := flags.String("destination", "", "absolute quarantine destination")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return writeError(stdout, operation, options.requestID, err)
	}
	if proposalFile.set == *proposalStdin {
		return writeError(stdout, operation, options.requestID, invalidRequest("proposal_source", "select exactly one of --proposal-file or --proposal-stdin"))
	}
	if *actionID == "" || *databaseID == "" {
		return writeError(stdout, operation, options.requestID, invalidRequest("precondition", "action ID and expected database ID are required"))
	}
	generation, err := strconv.ParseUint(*generationText, 10, 64)
	if err != nil || generation == 0 {
		return writeError(stdout, operation, options.requestID, invalidRequest("expected-manifest-generation", "must be greater than zero"))
	}
	if *destination == "" || !filepath.IsAbs(*destination) || filepath.Clean(*destination) != *destination || filepath.Clean(options.database) == filepath.Clean(*destination) {
		return writeError(stdout, operation, options.requestID, invalidRequest("destination", "must be a distinct clean absolute path"))
	}
	var reader io.Reader = stdin
	if proposalFile.set {
		opened, openErr := os.Open(proposalFile.value)
		if openErr != nil {
			return writeError(stdout, operation, options.requestID, invalidRequest("proposal-file", "cannot open proposal file"))
		}
		defer opened.Close()
		reader = opened
	}
	payload, readErr := readBounded(reader, maxProposalBytes, func(int) error {
		return invalidRequest("proposal", "proposal exceeds the 65536-byte limit")
	})
	if readErr != nil {
		return writeError(stdout, operation, options.requestID, readErr)
	}
	proposal, decodeErr := decodeProposal(payload)
	if decodeErr != nil {
		return writeError(stdout, operation, options.requestID, decodeErr)
	}
	if proposal.ID != *actionID || proposal.DatabaseID != *databaseID || proposal.ManifestGeneration != generation {
		return writeError(stdout, operation, options.requestID, recoveryGuardMismatch(proposal, *databaseID, generation))
	}
	result, applyErr := admin.ApplyRecovery(fsys, options.database, proposal, admin.ApplyOptions{ExpectedDatabaseID: *databaseID, ExpectedManifestGeneration: generation, QuarantineDirectory: *destination, RequestID: options.requestID, Timestamp: time.Now().UTC()})
	if applyErr != nil {
		return writeErrorEvidence(stdout, operation, options.requestID, applyErr, applyEvidence(result))
	}
	return writeSuccess(stdout, operation, options.requestID, result)
}

func decodeProposal(payload []byte) (admin.RecoveryProposal, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return admin.RecoveryProposal{}, invalidRequest("proposal", "proposal JSON is empty")
	}
	envelope := append([]byte(`{"schema_version":1,"ok":true,"database_id":"offline","operation":"offline_recover_propose","result":`), payload...)
	envelope = append(envelope, '}')
	response, err := protocol.DecodeResponse(envelope, maxProposalBytes+1024)
	if err != nil {
		return admin.RecoveryProposal{}, err
	}
	raw, ok := response.Result.(json.RawMessage)
	if !ok {
		return admin.RecoveryProposal{}, invalidRequest("proposal", "proposal JSON shape is invalid")
	}
	var proposal admin.RecoveryProposal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return admin.RecoveryProposal{}, invalidRequest("proposal", err.Error())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return admin.RecoveryProposal{}, invalidRequest("proposal", "proposal has trailing JSON")
	}
	return proposal, nil
}

func recoveryGuardMismatch(proposal admin.RecoveryProposal, databaseID string, generation uint64) *gapdb.Error {
	expectedGeneration, actualGeneration := generation, proposal.ManifestGeneration
	return &gapdb.Error{Code: gapdb.CodeRecoveryActionMismatch, Message: "Recovery guards do not match the proposal.", Retry: gapdb.RetryAfterReconcile, ProposalID: proposal.ID, ExpectedDatabaseID: databaseID, ActualDatabaseID: proposal.DatabaseID, ExpectedManifestGeneration: &expectedGeneration, ActualManifestGeneration: &actualGeneration, SafeActions: []gapdb.SafeAction{gapdb.ActionRecoverPropose, gapdb.ActionAbort}}
}

func inspectionEvidence(report admin.Inspection) any {
	if report.SchemaVersion == 0 {
		return nil
	}
	return report
}

func applyEvidence(result admin.ApplyResult) any {
	if result.ProposalID == "" && !result.OperationApplied {
		return nil
	}
	return result
}
