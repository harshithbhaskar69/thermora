package sim_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/sim"
)

func TestRunOperationsFenceOldCommandsAndRetries(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := sim.NewState(1)
	op := protocol.RunOperation{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      uuid.NewString(),
		RunID:         uuid.NewString(),
		Epoch:         1,
		Sequence:      1,
		Action:        "pause",
		Rate:          10,
		Model:         "heating",
		Expires:       now.Add(30 * time.Second),
	}
	if err := op.Validate(op.DeviceID, now); err != nil {
		t.Fatal(err)
	}
	f, err := op.Frame()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyRun(f); err != nil || !s.Paused || s.Rate != 10 {
		t.Fatalf("pause: %+v %v", s, err)
	}
	s.Elapsed = 120
	if err = s.ApplyRun(f); err != nil || s.Elapsed != 120 {
		t.Fatal("retry changed equipment", err)
	}
	conflicting := f
	conflicting[13] = 60
	if err = s.ApplyRun(conflicting); err == nil {
		t.Fatal("conflicting sequence accepted")
	}
	op.Sequence++
	op.ID = uuid.NewString()
	op.Action = "reset"
	op.Epoch = 2
	f, err = op.Frame()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyRun(f); err != nil || s.Elapsed != 0 || s.Paused || s.Epoch != 2 {
		t.Fatal("reset failed", err)
	}
	if err = s.Apply(frame(42), now); err == nil {
		t.Fatal("old run command survived reset")
	}
	s.Elapsed = 50
	if err = s.ApplyRun(f); err != nil || s.Elapsed != 50 {
		t.Fatal("reset replay reset twice", err)
	}
	if err = op.Validate(op.DeviceID, now.Add(31*time.Second)); err == nil {
		t.Fatal("expired operation accepted")
	}
}
