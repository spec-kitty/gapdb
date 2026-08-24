package gapdb

import "fmt"

const (
	DefaultMaxKeyBytes          = 4 << 10
	DefaultMaxValueBytes        = 8 << 20
	DefaultMaxFrameBytes        = 16 << 20
	DefaultMaxBatchBytes        = 16 << 20
	DefaultMaxBatchOperations   = 1024
	DefaultMaxScanRecords       = 1000
	DefaultMaxScanBytes         = 16 << 20
	DefaultWatchBufferEvents    = 256
	DefaultMaxWatchClients      = 256
	DefaultMaxConcurrentClients = 256
	DefaultMaxHistoryEvents     = 100_000
	DefaultMaxHistoryBytes      = 64 << 20
)

// Hard ceilings keep configurable resource bounds within values that have been
// reviewed for the local single-process design. Raising one is a format and
// resource-safety decision, not a routine configuration change.
const (
	HardMaxKeyBytes          = 64 << 10
	HardMaxValueBytes        = 64 << 20
	HardMaxFrameBytes        = 64 << 20
	HardMaxBatchBytes        = 64 << 20
	HardMaxBatchOperations   = 65_536
	HardMaxScanRecords       = 10_000
	HardMaxScanBytes         = 64 << 20
	HardMaxWatchBufferEvents = 8192
	HardMaxWatchClients      = 4096
	HardMaxConcurrentClients = 4096
	HardMaxHistoryEvents     = 1_000_000
	HardMaxHistoryBytes      = 1 << 30
)

// Limits bounds every caller-controlled or retained collection in the MVP.
type Limits struct {
	MaxKeyBytes          int `json:"max_key_bytes"`
	MaxValueBytes        int `json:"max_value_bytes"`
	MaxFrameBytes        int `json:"max_frame_bytes"`
	MaxBatchBytes        int `json:"max_batch_bytes"`
	MaxBatchOperations   int `json:"max_batch_operations"`
	MaxScanRecords       int `json:"max_scan_records"`
	MaxScanBytes         int `json:"max_scan_bytes"`
	WatchBufferEvents    int `json:"watch_buffer_events"`
	MaxWatchClients      int `json:"max_watch_clients"`
	MaxConcurrentClients int `json:"max_concurrent_clients"`
	MaxHistoryEvents     int `json:"max_history_events"`
	MaxHistoryBytes      int `json:"max_history_bytes"`
}

// Options contains startup configuration shared by public clients and the
// owner. The zero value is deliberately invalid so omitted bounds cannot become
// accidental unbounded behavior.
type Options struct {
	Limits Limits `json:"limits"`
}

func DefaultOptions() Options {
	return Options{Limits: Limits{
		MaxKeyBytes:          DefaultMaxKeyBytes,
		MaxValueBytes:        DefaultMaxValueBytes,
		MaxFrameBytes:        DefaultMaxFrameBytes,
		MaxBatchBytes:        DefaultMaxBatchBytes,
		MaxBatchOperations:   DefaultMaxBatchOperations,
		MaxScanRecords:       DefaultMaxScanRecords,
		MaxScanBytes:         DefaultMaxScanBytes,
		WatchBufferEvents:    DefaultWatchBufferEvents,
		MaxWatchClients:      DefaultMaxWatchClients,
		MaxConcurrentClients: DefaultMaxConcurrentClients,
		MaxHistoryEvents:     DefaultMaxHistoryEvents,
		MaxHistoryBytes:      DefaultMaxHistoryBytes,
	}}
}

func (o Options) Validate() error {
	l := o.Limits
	checks := []struct {
		name    string
		value   int
		ceiling int
	}{
		{"limits.max_key_bytes", l.MaxKeyBytes, HardMaxKeyBytes},
		{"limits.max_value_bytes", l.MaxValueBytes, HardMaxValueBytes},
		{"limits.max_frame_bytes", l.MaxFrameBytes, HardMaxFrameBytes},
		{"limits.max_batch_bytes", l.MaxBatchBytes, HardMaxBatchBytes},
		{"limits.max_batch_operations", l.MaxBatchOperations, HardMaxBatchOperations},
		{"limits.max_scan_records", l.MaxScanRecords, HardMaxScanRecords},
		{"limits.max_scan_bytes", l.MaxScanBytes, HardMaxScanBytes},
		{"limits.watch_buffer_events", l.WatchBufferEvents, HardMaxWatchBufferEvents},
		{"limits.max_watch_clients", l.MaxWatchClients, HardMaxWatchClients},
		{"limits.max_concurrent_clients", l.MaxConcurrentClients, HardMaxConcurrentClients},
		{"limits.max_history_events", l.MaxHistoryEvents, HardMaxHistoryEvents},
		{"limits.max_history_bytes", l.MaxHistoryBytes, HardMaxHistoryBytes},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.ceiling {
			return invalidField(check.name, fmt.Sprintf("must be between 1 and %d", check.ceiling))
		}
	}
	if l.MaxBatchBytes > l.MaxFrameBytes {
		return invalidField("limits.max_batch_bytes", "must not exceed max_frame_bytes")
	}
	if l.MaxScanBytes > l.MaxFrameBytes {
		return invalidField("limits.max_scan_bytes", "must not exceed max_frame_bytes")
	}
	if l.MaxHistoryEvents < l.WatchBufferEvents {
		return invalidField("limits.max_history_events", "must be at least watch_buffer_events")
	}
	if l.MaxWatchClients > l.MaxConcurrentClients {
		return invalidField("limits.max_watch_clients", "must not exceed max_concurrent_clients")
	}
	return nil
}
