package edge

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/simonvetter/modbus"
	bolt "go.etcd.io/bbolt"

	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/sim"
)

func TestJournalSurvivesRestartAndRejectsReplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "edge.db")
	s, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := protocol.NewConfiguration(uuid.NewString(), uuid.NewString(), protocol.Comfort{Target: 21, Min: 20, Max: 22})
	a := &agent{
		state:      s,
		cfg:        local.EdgeConfig{DeviceID: cfg.DeviceID, RunID: cfg.RunID},
		config:     cfg,
		epoch:      1,
		haveSample: true,
		sample:     protocol.Sample{ConfigVersion: 1},
		mqtt:       &messaging.Client{},
	}
	now := time.Now().UTC()
	c := protocol.Command{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      cfg.DeviceID,
		RunID:         cfg.RunID,
		RunEpoch:      1,
		Sequence:      2,
		ConfigVersion: 1,
		Generation:    1,
		Kind:          "set_heating_policy",
		Parameters:    protocol.Parameters{Mode: "remote", Room: 21.5, Supply: 42},
		IssuedAt:      now,
		ExpiresAt:     now.Add(30 * time.Second),
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(time.Hour),
	}
	// Simulate a crash immediately after receipt, before validation/acceptance.
	if err = s.event(c, "received", "", 0, nil); err != nil {
		t.Fatal(err)
	}
	send := func(ctx context.Context, c protocol.Command) error {
		t.Helper()
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return a.command(ctx, data)
	}
	if err = send(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err = send(t.Context(), c); err != nil {
		t.Fatal("duplicate not idempotent", err)
	}
	changed := c
	changed.Parameters.Supply = 43
	if err = send(t.Context(), changed); err == nil {
		t.Fatal("command ID conflict accepted")
	}
	replay := c
	replay.ID = uuid.NewString()
	replay.Sequence = 1
	if err = send(t.Context(), replay); err != nil {
		t.Fatal(err)
	}
	var record journalEntry
	if _, err = s.read("commands", replay.ID, &record); err != nil || record.Stage != "rejected" {
		t.Fatalf("old command not rejected: %+v %v", record, err)
	}
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openState(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = s.read(
		"commands",
		c.ID,
		&record,
	); err != nil || record.Stage != "accepted" ||
		record.Command.ID != c.ID {
		t.Fatalf("accepted command lost: %+v %v", record, err)
	}
	wantFrame := c.Frame(cfg)
	if record.Frame != wantFrame {
		t.Fatal("accepted frame was not durably recorded")
	}
	changedConfig := cfg
	changedConfig.Comfort.Target = 22
	if record.Frame == c.Frame(changedConfig) {
		t.Fatal("test must distinguish old accepted frame from current configuration")
	}
	events, err := s.pending("events", 10)
	if err != nil || len(events) < 3 {
		t.Fatalf("unacknowledged evidence lost: %d %v", len(events), err)
	}
}

func TestRecoverAppliedFrameAndSampleAtWallTimeBound(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "tcp://" + listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	equipment, err := sim.Open(filepath.Join(t.TempDir(), "sim.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := equipment.Close(); err != nil {
			t.Error(err)
		}
	})
	server, err := modbus.NewServer(&modbus.ServerConfiguration{URL: address}, equipment)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := modbus.NewClient(&modbus.ClientConfiguration{URL: address, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetUnitId(1); err != nil {
		t.Fatal(err)
	}
	if err = client.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(t.TempDir(), "edge.db")
	state, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := protocol.NewConfiguration(uuid.NewString(), uuid.NewString(), protocol.Comfort{Target: 21, Min: 20, Max: 22})
	now := time.Now()
	command := protocol.Command{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      cfg.DeviceID,
		RunID:         cfg.RunID,
		RunEpoch:      1,
		Sequence:      1,
		Generation:    1,
		ConfigVersion: 1,
		IssuedAt:      now.UTC(),
		ExpiresAt:     now.Add(30 * time.Second).UTC(),
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(time.Hour),
		Kind:          "set_heating_policy",
		Parameters:    protocol.Parameters{Mode: "remote", Room: 22, Supply: 42},
	}
	frame := command.Frame(cfg)
	if err = state.event(command, "accepted", "", 0, &frame); err != nil {
		t.Fatal(err)
	}
	if err = client.WriteRegisters(0, frame[:]); err != nil {
		t.Fatal(err)
	}
	// Crash after the equipment write, before recording application in the edge journal.
	if err = state.db.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = openState(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.db.Close(); err != nil {
			t.Error(err)
		}
	})
	var recovered journalEntry
	if _, err = state.read("commands", command.ID, &recovered); err != nil {
		t.Fatal(err)
	}
	cfg.Comfort.Target, cfg.Version, cfg.Generation = 22, 2, 2
	a := &agent{
		state:     state,
		cfg:       local.EdgeConfig{DeviceID: cfg.DeviceID, RunID: cfg.RunID, RunEpoch: 1},
		config:    cfg,
		active:    &recovered,
		mqtt:      &messaging.Client{},
		modbus:    client,
		connected: true,
		boot:      uuid.NewString(),
	}
	for _, elapsed := range []time.Duration{0, time.Second, 6 * time.Second} {
		if err = a.poll(t.Context(), now.Add(elapsed)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = state.read(
		"commands",
		command.ID,
		&recovered,
	); err != nil || recovered.Stage != "applied" ||
		recovered.Frame != frame {
		t.Fatalf("readback failed to reconcile the original command frame: %v", err)
	}
	batches, err := state.pending("batches", 10)
	if err != nil || len(batches) != 2 {
		t.Fatalf("wall-time sampling bound not respected: %d, %v", len(batches), err)
	}
	sequences := map[uint32]bool{}
	for _, message := range batches {
		var batch protocol.Batch
		if err = json.Unmarshal(message.data, &batch); err != nil {
			t.Fatal(err)
		}
		sequences[batch.Samples[0].Sequence] = true
	}
	if !sequences[1] || !sequences[2] {
		t.Fatal("samples at the same simulation instant reused their identity")
	}
}

func TestApplicationReceiptOwnsSpoolDeletion(t *testing.T) {
	t.Parallel()
	s, err := openState(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.db.Close(); err != nil {
			t.Error(err)
		}
	})
	b := protocol.Batch{ID: uuid.NewString(), Samples: []protocol.Sample{{Elapsed: 60}}}
	if err = s.batch(b); err != nil {
		t.Fatal(err)
	}
	if err = s.ackBatch(uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	pending, err := s.pending("batches", 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("unmatched ACK removed data")
	}
	if err = s.ackBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = s.pending("batches", 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("committed ACK did not remove batch")
	}
}

func TestFullCommandJournalPreservesEvidence(t *testing.T) {
	s, err := openState(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.db.Close(); err != nil {
			t.Error(err)
		}
	})
	err = s.db.Update(func(tx *bolt.Tx) error {
		for i := range 10000 {
			if err := tx.Bucket([]byte("commands")).Put([]byte(strconv.Itoa(i)), []byte(`{}`)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	command := protocol.Command{ID: uuid.NewString()}
	if err = s.event(command, "received", "", 0, nil); err == nil {
		t.Fatal("full journal accepted new remote work")
	}
	diagnostics, err := s.diagnostics(time.Now())
	if err != nil || diagnostics.JournalEntries != 10000 || diagnostics.PendingEvents != 0 {
		t.Fatal("rejection damaged existing journal", diagnostics, err)
	}
}
