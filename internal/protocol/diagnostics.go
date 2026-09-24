package protocol

import "time"

// Hello reports connectivity and bounded journal occupancy, never equipment execution.
type Hello struct {
	DeviceID    string       `json:"device_id"`
	RunID       string       `json:"run_id"`
	BootID      string       `json:"boot_id"`
	Diagnostics *Diagnostics `json:"diagnostics,omitempty"`
}

type Diagnostics struct {
	RecordedAt      time.Time `json:"recorded_at_wall"`
	BufferedBatches int       `json:"buffered_batches"`
	BufferedBytes   int       `json:"buffered_bytes"`
	PendingEvents   int       `json:"pending_events"`
	JournalEntries  int       `json:"journal_entries"`
}
