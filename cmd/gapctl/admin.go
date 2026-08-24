package main

import (
	"context"
	"io"
	"path/filepath"
	"strconv"

	"gapdb/gapdb"
)

func runAdminCommand(options globalOptions, command string, arguments []string, stdin io.Reader, stdout io.Writer) int {
	if command == "inspect" || command == "recover-propose" || command == "recover-apply" || command == "verify" && options.database != "" {
		return runOfflineCommand(options, command, arguments, stdin, stdout)
	}
	result, err := runOnlineAdmin(options, command, arguments)
	if err != nil {
		return writeError(stdout, command, options.requestID, err)
	}
	return writeSuccess(stdout, command, options.requestID, result)
}

func runOnlineAdmin(options globalOptions, command string, arguments []string) (any, error) {
	switch command {
	case "status", "health", "stats", "describe-config":
		if len(arguments) != 0 {
			return nil, invalidRequest("arguments", command+" takes no arguments")
		}
		return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
			switch command {
			case "status":
				return client.Status(ctx)
			case "health":
				return client.Health(ctx)
			case "stats":
				return client.Stats(ctx)
			default:
				return client.DescribeConfig(ctx)
			}
		})
	case "verify":
		flags := commandFlags(command)
		mode := flags.String("mode", "sampled", "sampled or full")
		if err := parseCommandFlags(flags, arguments); err != nil {
			return nil, err
		}
		if *mode != "sampled" && *mode != "full" {
			return nil, invalidRequest("mode", "must be sampled or full")
		}
		return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
			return client.Verify(ctx, *mode)
		})
	case "create-snapshot":
		flags := commandFlags(command)
		databaseID := flags.String("expected-database-id", "", "exact database ID")
		revisionText := flags.String("expected-revision", "", "exact current revision")
		if err := parseCommandFlags(flags, arguments); err != nil {
			return nil, err
		}
		revision, err := parseRevisionIncludingZero("expected-revision", *revisionText)
		if err != nil || *databaseID == "" {
			if err != nil {
				return nil, err
			}
			return nil, invalidRequest("expected-database-id", "is required")
		}
		return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
			return client.CreateSnapshot(ctx, *databaseID, revision)
		})
	case "compact":
		flags := commandFlags(command)
		databaseID := flags.String("expected-database-id", "", "exact database ID")
		throughText := flags.String("through-revision", "", "exact compaction boundary")
		if err := parseCommandFlags(flags, arguments); err != nil {
			return nil, err
		}
		through, err := parseRevisionIncludingZero("through-revision", *throughText)
		if err != nil || *databaseID == "" {
			if err != nil {
				return nil, err
			}
			return nil, invalidRequest("expected-database-id", "is required")
		}
		return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
			return client.Compact(ctx, *databaseID, through)
		})
	case "backup":
		flags := commandFlags(command)
		databaseID := flags.String("expected-database-id", "", "exact database ID")
		revisionText := flags.String("expected-revision", "", "exact durable revision")
		destination := flags.String("destination", "", "absolute new backup destination")
		if err := parseCommandFlags(flags, arguments); err != nil {
			return nil, err
		}
		revision, err := parseRevisionIncludingZero("expected-revision", *revisionText)
		if err != nil {
			return nil, err
		}
		if *databaseID == "" {
			return nil, invalidRequest("expected-database-id", "is required")
		}
		if *destination == "" || !filepath.IsAbs(*destination) || filepath.Clean(*destination) != *destination {
			return nil, invalidRequest("destination", "must be a clean absolute path")
		}
		return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
			return client.Backup(ctx, *databaseID, revision, *destination)
		})
	default:
		return nil, invalidRequest("command", "unknown administrative command")
	}
}

func parseRevisionIncludingZero(field, text string) (gapdb.Revision, error) {
	if text == "" {
		return 0, invalidRequest(field, "is required")
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, invalidRequest(field, "must be a base-10 integer including zero")
	}
	return gapdb.Revision(value), nil
}
