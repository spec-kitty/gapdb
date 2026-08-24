package admin

import (
	"errors"

	"gapdb/gapdb"
)

// RunningView is a bounded model-friendly administrative status surface.
type RunningView struct {
	SchemaVersion   uint16          `json:"schema_version"`
	Status          gapdb.Status    `json:"status"`
	Healthy         bool            `json:"healthy"`
	DegradedReason  string          `json:"degraded_reason,omitempty"`
	RecordCount     int             `json:"record_count"`
	WatchCount      int             `json:"watch_count"`
	QueueDepth      int             `json:"queue_depth"`
	EffectiveConfig EffectiveConfig `json:"effective_config"`
}

type EffectiveConfig struct {
	Limits                gapdb.Limits `json:"limits"`
	MutationQueueCapacity int          `json:"mutation_queue_capacity"`
	WALBufferBytes        int          `json:"wal_buffer_bytes"`
	AuditMaxLineBytes     int          `json:"audit_max_line_bytes"`
	AuditMaxFileBytes     int64        `json:"audit_max_file_bytes"`
	AuditKeepGenerations  int          `json:"audit_keep_generations"`
	MaxAdminPaths         int          `json:"max_admin_paths"`
}

type RunningSource interface {
	AdminStatus() gapdb.Status
	AdminCounts() (records, watches, queueDepth, queueCapacity int)
}

func Status(source RunningSource) RunningView {
	status := source.AdminStatus()
	records, watches, queueDepth, queueCapacity := source.AdminCounts()
	view := RunningView{SchemaVersion: 1, Status: status, Healthy: status.Lifecycle == gapdb.LifecycleReady, RecordCount: records, WatchCount: watches, QueueDepth: queueDepth, EffectiveConfig: EffectiveConfig{Limits: status.Limits, MutationQueueCapacity: queueCapacity}}
	if !view.Healthy {
		view.DegradedReason = string(status.Lifecycle)
	}
	return view
}

type Inspection struct {
	SchemaVersion      uint16          `json:"schema_version"`
	DatabaseID         string          `json:"database_id,omitempty"`
	ManifestGeneration uint64          `json:"manifest_generation,omitempty"`
	SnapshotRevision   gapdb.Revision  `json:"snapshot_revision,omitempty"`
	CurrentRevision    gapdb.Revision  `json:"current_revision,omitempty"`
	FindingCode        gapdb.ErrorCode `json:"finding_code,omitempty"`
	FindingFile        string          `json:"finding_file,omitempty"`
	FindingStage       string          `json:"finding_stage,omitempty"`
	EvidenceSHA256     string          `json:"evidence_sha256,omitempty"`
	EvidenceSize       int64           `json:"evidence_size,omitempty"`
	EvidenceDevice     uint64          `json:"evidence_device,omitempty"`
	EvidenceInode      uint64          `json:"evidence_inode,omitempty"`
	Verified           bool            `json:"verified"`
}

func inspectionError(report Inspection, err error) (Inspection, error) {
	var structured *gapdb.Error
	if errors.As(err, &structured) {
		report.FindingCode = structured.Code
		report.FindingFile = structured.File
		if report.FindingFile == "" {
			report.FindingFile = structured.Path
		}
		report.FindingStage = structured.FailedStage
	}
	return report, err
}
