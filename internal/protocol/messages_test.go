package protocol_test

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/protocol"
)

func TestCommandValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := protocol.NewConfiguration(uuid.NewString(), uuid.NewString(), protocol.Comfort{Target: 21, Min: 20, Max: 22})
	valid := protocol.Command{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      cfg.DeviceID,
		RunID:         cfg.RunID,
		RunEpoch:      1,
		Sequence:      1,
		ConfigVersion: 1,
		Generation:    1,
		Kind:          "set_heating_policy",
		Parameters:    protocol.Parameters{Mode: "remote", Room: 21.5, Supply: 42},
		IssuedAt:      now,
		ExpiresAt:     now.Add(30 * time.Second),
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(time.Hour),
	}
	for _, tc := range []struct {
		name   string
		change func(*protocol.Command)
		valid  bool
	}{
		{"current", func(*protocol.Command) {}, true},
		{"expired", func(c *protocol.Command) { c.ExpiresAt = now }, false},
		{"wrong run", func(c *protocol.Command) { c.RunID = uuid.NewString() }, false},
		{"old generation", func(c *protocol.Command) { c.Generation = 0 }, false},
		{"unsafe supply", func(c *protocol.Command) { c.Parameters.Supply = 60 }, false},
		{"not finite", func(c *protocol.Command) { c.Parameters.Room = math.NaN() }, false},
		{"elapsed window", func(c *protocol.Command) { c.ValidUntil = protocol.StartTime }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := valid
			tc.change(&c)
			err := c.Validate(cfg, 1, 1, 0, now)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	if err := valid.Validate(cfg, 1, 0, 0, now); err == nil {
		t.Fatal("remote command accepted without equipment configuration")
	}
}

func TestDecodeRejectsOversizeAndTrailingData(t *testing.T) {
	t.Parallel()
	var value any
	for _, input := range [][]byte{make([]byte, protocol.MaxMessageBytes+1), []byte(`{} {}`), []byte(`{"n":NaN}`)} {
		if err := protocol.Decode(input, &value); err == nil {
			t.Fatal("accepted invalid message")
		}
	}
}

func TestCommandFrameValidation(t *testing.T) {
	t.Parallel()
	cfg := protocol.NewConfiguration(uuid.NewString(), uuid.NewString(), protocol.Comfort{Target: 21, Min: 20, Max: 22})
	command := protocol.Command{
		RunEpoch:      1,
		Sequence:      1,
		Generation:    1,
		ConfigVersion: 1,
		Parameters:    protocol.Parameters{Mode: "remote", Room: 21, Supply: 40},
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(time.Minute),
	}
	valid := command.Frame(cfg)
	if err := valid.Validate(1, 0); err != nil {
		t.Fatalf("Frame.Validate at start of valid window = %v, want nil", err)
	}
	if err := valid.Validate(1, 60); err == nil {
		t.Error("Frame.Validate at end of valid window = nil, want expired frame error")
	}
	for name, change := range map[string]func(*protocol.Frame){
		"wrong epoch":      func(f *protocol.Frame) { protocol.Put32(f[:2], 2) },
		"zero sequence":    func(f *protocol.Frame) { protocol.Put32(f[2:4], 0) },
		"room too cold":    func(f *protocol.Frame) { f[8] = protocol.Centi(19) },
		"supply too hot":   func(f *protocol.Frame) { f[9] = protocol.Centi(51) },
		"unsupported mode": func(f *protocol.Frame) { f[13] = 2 },
		"future window":    func(f *protocol.Frame) { protocol.Put32(f[16:18], 10) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			frame := valid
			change(&frame)
			if err := frame.Validate(1, 0); err == nil {
				t.Errorf("Frame.Validate(%s) = nil, want validation error", name)
			}
		})
	}
}
