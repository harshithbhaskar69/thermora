package protocol_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"thermora.local/thermora/internal/protocol"
)

func TestMQTTWireSchemas(t *testing.T) {
	t.Parallel()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile("../../api/mqtt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cfg := protocol.NewConfiguration(uuid.NewString(), uuid.NewString(), protocol.Comfort{Target: 21, Min: 20, Max: 22})
	command := protocol.Command{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      cfg.DeviceID,
		RunID:         cfg.RunID,
		RunEpoch:      1,
		Sequence:      1,
		Generation:    1,
		ConfigVersion: 1,
		IssuedAt:      now,
		ExpiresAt:     now.Add(30 * time.Second),
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(time.Hour),
		Kind:          "set_heating_policy",
		Source:        "manual_override",
		Reason:        "owner_override",
		CorrelationID: uuid.NewString(),
		Parameters:    protocol.Parameters{Mode: "remote", Room: 22, Supply: 42},
	}
	batch := protocol.Batch{
		SchemaVersion: 1,
		DeviceID:      cfg.DeviceID,
		RunID:         cfg.RunID,
		BootID:        uuid.NewString(),
		ID:            uuid.NewString(),
		Origin:        "simulation",
		SentAt:        now,
		Samples: []protocol.Sample{{
			Sequence: 1, Elapsed: 60, ObservedAt: protocol.StartTime.Add(time.Minute), RecordedAt: now,
			Indoor: 21, Outdoor: -3, Supply: 40, RoomTarget: 22, SupplyTarget: 42,
			ElectricalW: 2000, ThermalW: 6000, EnergyWh: 34, Compressor: "running", Mode: "remote",
			Alarms: []string{}, Quality: []string{}, AppliedSeq: 1, ConfigVersion: 1,
		}},
	}
	if err = batch.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = command.Validate(cfg, 1, 1, 0, now); err != nil {
		t.Fatal(err)
	}
	for name, message := range map[string]any{
		"Hello":         protocol.Hello{DeviceID: cfg.DeviceID, RunID: cfg.RunID, BootID: uuid.NewString(), Diagnostics: &protocol.Diagnostics{RecordedAt: now}},
		"Configuration": cfg,
		"RunOperation":  protocol.RunOperation{SchemaVersion: 1, ID: uuid.NewString(), DeviceID: cfg.DeviceID, RunID: cfg.RunID, Epoch: 1, Sequence: 1, Action: "pause", Rate: 60, Model: "heating", Expires: now.Add(30 * time.Second)},
		"RunReport":     protocol.RunReport{SchemaVersion: 1, ID: uuid.NewString(), DeviceID: cfg.DeviceID, RunID: cfg.RunID, Epoch: 1, Sequence: 1},
		"ConfigReport":  protocol.ConfigReport{SchemaVersion: 1, DeviceID: cfg.DeviceID, RunID: cfg.RunID, Desired: 1, Accepted: 1, Reported: 1, Status: "applied", ReportedAt: now},
		"Command":       command,
		"Lease":         protocol.Lease{SchemaVersion: 1, DeviceID: cfg.DeviceID, RunID: cfg.RunID, Generation: 1, Sequence: 1, IssuedAt: now, ValidUntil: now.Add(30 * time.Second)},
		"Event":         protocol.Event{SchemaVersion: 1, ID: uuid.NewString(), CommandID: command.ID, DeviceID: cfg.DeviceID, RunID: cfg.RunID, Stage: "applied", OccurredAt: now, ObservedAt: protocol.StartTime},
		"Batch":         batch,
		"Receipt":       protocol.Receipt{SchemaVersion: 1, BatchID: batch.ID, Status: "committed", ReceivedAt: now},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err = json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			schema := doc.Components.Schemas[name].Value
			if err = schema.VisitJSON(value); err != nil {
				t.Fatal(err)
			}
			// Hello uses the v1 topic contract without a payload version field.
			if name == "Hello" {
				delete(value, "device_id")
				if err = schema.VisitJSON(value); err == nil {
					t.Fatal("accepted hello without a device identity")
				}
				return
			}
			value["schema_version"] = float64(2)
			if err = schema.VisitJSON(value); err == nil {
				t.Fatal("accepted unknown major schema version")
			}
			delete(value, "schema_version")
			if err = schema.VisitJSON(value); err == nil {
				t.Fatal("accepted missing schema version")
			}
		})
	}
	for name, change := range map[string]func(*protocol.Sample){
		"out of range":           func(s *protocol.Sample) { s.Supply = 100 },
		"unknown quality":        func(s *protocol.Sample) { s.Quality = []string{"unknown"} },
		"unknown compressor":     func(s *protocol.Sample) { s.Compressor = "unknown" },
		"missing wall timestamp": func(s *protocol.Sample) { s.RecordedAt = time.Time{} },
		"zero sequence":          func(s *protocol.Sample) { s.Sequence = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			invalid := batch
			invalid.Samples = append([]protocol.Sample(nil), batch.Samples...)
			change(&invalid.Samples[0])
			if err := invalid.Validate(); err == nil {
				t.Fatal("accepted invalid telemetry")
			}
		})
	}
}
