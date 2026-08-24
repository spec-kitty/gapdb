package protocol

import (
	"fmt"

	"gapdb/gapdb"
)

func invalidProtocol(field, reason string, cause error) error {
	return &gapdb.Error{
		Code:        gapdb.CodeInvalidRequest,
		Message:     "Protocol request is invalid.",
		Retry:       gapdb.RetryNever,
		Field:       field,
		Reason:      reason,
		SafeActions: []gapdb.SafeAction{gapdb.ActionFixRequest, gapdb.ActionAbort},
		Cause:       cause,
	}
}

func unsupportedVersion(received uint64) error {
	return &gapdb.Error{
		Code:              gapdb.CodeUnsupportedVersion,
		Message:           "Protocol version is not supported.",
		Retry:             gapdb.RetryNever,
		ReceivedVersion:   &received,
		SupportedVersions: []uint64{SchemaVersion},
		SafeActions:       []gapdb.SafeAction{gapdb.ActionUseSupportedVersion, gapdb.ActionUpgradeClient, gapdb.ActionAbort},
	}
}

func frameTooLarge(received, maximum int) error {
	return &gapdb.Error{
		Code:          gapdb.CodeFrameTooLarge,
		Message:       "Protocol frame exceeds the configured limit.",
		Retry:         gapdb.RetryNever,
		ReceivedBytes: received,
		MaximumBytes:  maximum,
		SafeActions:   []gapdb.SafeAction{gapdb.ActionReduceRequest, gapdb.ActionAbort},
	}
}

func typeReason(value any) string {
	return fmt.Sprintf("unexpected arguments type %T", value)
}
