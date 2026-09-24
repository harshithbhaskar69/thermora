package protocol_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/protocol"
)

func TestRunOperationValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	valid := protocol.RunOperation{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      uuid.NewString(),
		RunID:         uuid.NewString(),
		Epoch:         1,
		Sequence:      1,
		Action:        "pause",
		Rate:          1,
		Model:         "heating",
		Expires:       now.Add(30 * time.Second),
	}
	for _, tc := range []struct {
		name   string
		change func(*protocol.RunOperation)
		valid  bool
	}{
		{name: "current", change: func(*protocol.RunOperation) {}, valid: true},
		{name: "maximum lifetime", change: func(o *protocol.RunOperation) { o.Expires = now.Add(35 * time.Second) }, valid: true},
		{name: "resume at 10x", change: func(o *protocol.RunOperation) { o.Action, o.Rate = "resume", 10 }, valid: true},
		{name: "rate at 60x", change: func(o *protocol.RunOperation) { o.Action, o.Rate = "rate", 60 }, valid: true},
		{name: "reset with hot water", change: func(o *protocol.RunOperation) { o.Action, o.Model = "reset", "heating-dhw" }, valid: true},
		{name: "invalid operation ID", change: func(o *protocol.RunOperation) { o.ID = "invalid" }},
		{name: "invalid device ID", change: func(o *protocol.RunOperation) { o.DeviceID = "invalid" }},
		{name: "invalid run ID", change: func(o *protocol.RunOperation) { o.RunID = "invalid" }},
		{name: "unknown schema", change: func(o *protocol.RunOperation) { o.SchemaVersion = 2 }},
		{name: "another device", change: func(o *protocol.RunOperation) { o.DeviceID = uuid.NewString() }},
		{name: "zero epoch", change: func(o *protocol.RunOperation) { o.Epoch = 0 }},
		{name: "zero sequence", change: func(o *protocol.RunOperation) { o.Sequence = 0 }},
		{name: "expired", change: func(o *protocol.RunOperation) { o.Expires = now.Add(-time.Nanosecond) }},
		{name: "expiry boundary", change: func(o *protocol.RunOperation) { o.Expires = now }},
		{name: "excessive lifetime", change: func(o *protocol.RunOperation) { o.Expires = now.Add(35*time.Second + time.Nanosecond) }},
		{name: "unsupported rate", change: func(o *protocol.RunOperation) { o.Rate = 2 }},
		{name: "unsupported model", change: func(o *protocol.RunOperation) { o.Model = "cooling" }},
		{name: "unsupported action", change: func(o *protocol.RunOperation) { o.Action = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op := valid
			tc.change(&op)
			if err := op.Validate(valid.DeviceID, now); (err == nil) != tc.valid {
				t.Errorf("RunOperation.Validate(%s) = %v, want valid=%v", tc.name, err, tc.valid)
			}
		})
	}
}
