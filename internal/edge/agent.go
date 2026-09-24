package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/simonvetter/modbus"

	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
)

type agent struct {
	sampled                           bool
	sampleCounter                     uint32
	lastSampleWall                    time.Time
	cfg                               local.EdgeConfig
	state                             *state
	identity                          identityState
	mqtt                              *messaging.Client
	modbus                            *modbus.ModbusClient
	connected                         bool
	config                            protocol.Configuration
	sample                            protocol.Sample
	epoch                             uint32
	boot                              string
	lastSample                        uint32
	haveSample                        bool
	active                            *journalEntry
	lease                             protocol.Lease
	leaseDeadline                     time.Time
	lastRenewal, lastFlush, lastHello time.Time
}

func Run(ctx context.Context, cfg local.EdgeConfig, path string) (err error) {
	s, err := openState(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.db.Close()) }()
	id, err := s.identity()
	if err != nil {
		return err
	}
	if id.Organization == "" {
		id, err = activate(ctx, cfg, s, id)
		if err != nil {
			return err
		}
	}
	prefix := func(suffix string) string { return protocol.Topic(id.Organization, cfg.DeviceID, suffix) }
	topics := []string{}
	for _, suffix := range []string{"command", "control/lease", "config/desired", "demo/action", "telemetry/receipt", "command/event/receipt"} {
		topics = append(topics, prefix(suffix))
	}
	will := messaging.Message{Topic: prefix("presence"), Payload: []byte(`{"status":"offline"}`)}
	mqtt, err := messaging.Connect(
		ctx,
		messaging.Settings{
			URL:      cfg.MQTTURL,
			Username: cfg.DeviceID,
			ClientID: cfg.DeviceID,
			Password: id.Password,
			CAFile:   cfg.CAFile,
		},
		topics,
		&will,
	)
	if err != nil {
		return err
	}
	client, err := modbus.NewClient(&modbus.ClientConfiguration{URL: cfg.ModbusURL, Timeout: time.Second})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	if err = client.SetUnitId(1); err != nil {
		return err
	}
	var current protocol.RunOperation
	if found, e := s.read("meta", "run-current", &current); e != nil {
		return e
	} else if found {
		cfg.RunID, cfg.RunEpoch = current.RunID, current.Epoch
	}
	a := &agent{cfg: cfg, state: s, identity: id, mqtt: mqtt, modbus: client, boot: uuid.NewString()}
	if _, err = s.read("meta", "config", &a.config); err != nil {
		return err
	}
	// An accepted journal entry is reconciled against readback before any retry.
	entries, err := s.pending("commands", 10000)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var j journalEntry
		if err = json.Unmarshal(entry.data, &j); err != nil {
			return err
		}
		if a.config.Version > 0 && j.Command.Generation == a.config.Generation && j.Command.RunID == a.cfg.RunID &&
			j.Stage == "accepted" &&
			(a.active == nil || j.Command.Sequence > a.active.Command.Sequence) {
			entry := j
			a.active = &entry
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-mqtt.Incoming:
			if err = a.message(ctx, msg); err != nil {
				slog.Warn("edge message rejected", "error", err)
			}
		case now := <-ticker.C:
			if err = a.poll(ctx, now); err != nil {
				slog.Warn("equipment sample unavailable", "error", err)
			}
			if mqtt.Ready.Load() && now.Sub(a.lastHello) >= 5*time.Second {
				diagnostics, diagErr := a.state.diagnostics(now)
				if diagErr != nil {
					return diagErr
				}
				if err = mqtt.JSON(
					ctx,
					prefix("hello"),
					protocol.Hello{
						DeviceID:    cfg.DeviceID,
						RunID:       a.cfg.RunID,
						BootID:      a.boot,
						Diagnostics: &diagnostics,
					},
					false,
				); err == nil {
					a.lastHello = now
				}
			}
			if now.Sub(a.lastFlush) >= 2*time.Second {
				if err = a.flush(ctx); err != nil {
					slog.Warn("edge delivery deferred", "error", err)
				}
				a.lastFlush = now
			}
		}
	}
}

func activate(ctx context.Context, cfg local.EdgeConfig, s *state, id identityState) (identityState, error) {
	tls, err := messaging.TLSConfig(cfg.CAFile)
	if err != nil {
		return id, err
	}
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: tls},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	body, err := json.Marshal(
		map[string]any{"device_id": cfg.DeviceID, "attempt_id": id.Attempt, "mqtt_password": id.Password},
	)
	if err != nil {
		return id, err
	}
	for {
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			cfg.CloudURL+"/edge/v1/enroll",
			bytes.NewReader(body),
		)
		if err != nil {
			return id, err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Bootstrap)
		req.Header.Set("Content-Type", "application/json")
		resp, callErr := client.Do(req)
		if callErr == nil {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
			_ = resp.Body.Close()
			if readErr == nil && len(data) <= 4096 && resp.StatusCode == 200 {
				var result struct {
					OrganizationID string `json:"organization_id"`
					DeviceID       string `json:"device_id"`
					RunID          string `json:"run_id"`
					Epoch          uint32 `json:"run_epoch"`
				}
				if err = json.Unmarshal(data, &result); err != nil {
					return id, err
				}
				if _, err = uuid.Parse(result.OrganizationID); err != nil {
					return id, err
				}
				if result.DeviceID != cfg.DeviceID || result.RunID != cfg.RunID || result.Epoch != cfg.RunEpoch {
					return id, errors.New("activation identity mismatch")
				}
				id.Organization = result.OrganizationID
				return id, s.put("meta", "identity", id)
			}
			if resp.StatusCode == 401 {
				return id, errors.New(
					"bootstrap credential invalid or expired; provision a fresh unclaimed demo identity",
				)
			}
		}
		select {
		case <-ctx.Done():
			return id, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *agent) message(ctx context.Context, msg messaging.Message) error {
	prefix := protocol.Topic(a.identity.Organization, a.cfg.DeviceID, "")
	if !strings.HasPrefix(msg.Topic, prefix) {
		return errors.New("wrong device topic")
	}
	switch strings.TrimPrefix(msg.Topic, prefix) {
	case "demo/action":
		return a.runOperation(ctx, msg.Payload)
	case "config/desired":
		var cfg protocol.Configuration
		if err := protocol.Decode(msg.Payload, &cfg); err != nil {
			return err
		}
		if err := cfg.Validate(a.cfg.DeviceID, a.cfg.RunID); err != nil {
			return err
		}
		if cfg.Version < a.config.Version || cfg.Generation < a.config.Generation {
			return errors.New("stale desired configuration")
		}
		if cfg.Version == a.config.Version && cfg.Generation == a.config.Generation {
			if cfg != a.config {
				return errors.New("conflicting desired configuration")
			}
			return a.report(ctx)
		}
		if err := a.state.put("meta", "config", cfg); err != nil {
			return err
		}
		a.config = cfg
		return a.report(ctx)
	case "command":
		return a.command(ctx, msg.Payload)
	case "control/lease":
		var lease protocol.Lease
		if err := protocol.Decode(msg.Payload, &lease); err != nil {
			return err
		}
		now := time.Now()
		if lease.SchemaVersion != 1 || lease.DeviceID != a.cfg.DeviceID || lease.RunID != a.cfg.RunID ||
			lease.Generation != a.config.Generation ||
			!lease.ValidUntil.After(now) ||
			lease.ValidUntil.Sub(lease.IssuedAt) > 30*time.Second ||
			!lease.ValidUntil.After(lease.IssuedAt) ||
			lease.IssuedAt.After(now.Add(5*time.Second)) ||
			lease.IssuedAt.Before(now.Add(-5*time.Second)) {
			return errors.New("invalid lease")
		}
		if lease.Sequence < a.lease.Sequence {
			return errors.New("stale lease")
		}
		a.lease = lease
		a.leaseDeadline = now.Add(lease.ValidUntil.Sub(now))
		return nil
	case "telemetry/receipt":
		var r protocol.Receipt
		if err := protocol.Decode(msg.Payload, &r); err != nil {
			return err
		}
		if r.SchemaVersion != 1 {
			return errors.New("unsupported receipt")
		}
		if r.Status == "committed" {
			return a.state.ackBatch(r.BatchID)
		}
		if r.Status == "rejected" {
			return fmt.Errorf("telemetry permanently rejected: %s", r.Code)
		}
	case "command/event/receipt":
		var r protocol.Receipt
		if err := protocol.Decode(msg.Payload, &r); err != nil {
			return err
		}
		if r.SchemaVersion == 1 && r.Status == "committed" {
			return a.state.remove("events", r.EventID)
		}
	}
	return nil
}

func (a *agent) command(ctx context.Context, data []byte) error {
	var c protocol.Command
	if err := protocol.Decode(data, &c); err != nil {
		return err
	}
	var old journalEntry
	found, err := a.state.read("commands", c.ID, &old)
	if err != nil {
		return err
	}
	if found {
		if old.Command != c {
			return errors.New("command ID reused with different content")
		}
		if old.Stage != "received" {
			return a.state.event(c, old.Stage, old.Reason, a.sample.Elapsed, nil)
		}
	}
	if !found {
		if err = a.state.event(c, "received", "", a.sample.Elapsed, nil); err != nil {
			return err
		}
	}
	if !a.haveSample {
		err = errors.New("equipment_unavailable")
	} else {
		err = c.Validate(a.config, a.epoch, a.sample.ConfigVersion, a.sample.Elapsed, time.Now())
	}
	if err == nil && c.Sequence <= a.sample.AppliedSeq {
		err = errors.New("replayed_command")
	}
	if err == nil && a.active != nil && c.Sequence <= a.active.Command.Sequence {
		err = errors.New("replayed_command")
	}
	if err != nil {
		return a.state.event(c, "rejected", err.Error(), a.sample.Elapsed, nil)
	}
	frame := c.Frame(a.config)
	if err = a.state.event(c, "accepted", "", a.sample.Elapsed, &frame); err != nil {
		return err
	}
	a.active = &journalEntry{Command: c, Stage: "accepted", Frame: frame}
	slog.Info("command accepted", "command_id", c.ID, "correlation_id", c.CorrelationID)
	return a.flush(ctx)
}

func (a *agent) report(ctx context.Context) error {
	if !a.mqtt.Ready.Load() {
		return nil // Reconcile desired/reported state again after the next hello.
	}
	status := "accepted"
	if a.sample.ConfigVersion == a.config.Version {
		status = "applied"
	}
	return a.mqtt.JSON(
		ctx,
		protocol.Topic(a.identity.Organization, a.cfg.DeviceID, "config/reported"),
		protocol.ConfigReport{
			SchemaVersion: 1,
			DeviceID:      a.cfg.DeviceID,
			RunID:         a.cfg.RunID,
			Desired:       a.config.Version,
			Accepted:      a.config.Version,
			Reported:      a.sample.ConfigVersion,
			Status:        status,
			ReportedAt:    time.Now().UTC(),
		},
		true,
	)
}

func (a *agent) poll(ctx context.Context, now time.Time) error {
	if !a.connected {
		if err := a.modbus.Open(); err != nil {
			return err
		}
		a.connected = true
	}
	r, err := a.modbus.ReadRegisters(0, protocol.InputCount, modbus.INPUT_REGISTER)
	if err != nil {
		_ = a.modbus.Close()
		a.connected = false
		return err
	}
	sample, err := protocol.SampleFromRegisters(r, now)
	if err != nil {
		return err
	}
	a.epoch = protocol.Uint32(r[1:3])
	if a.epoch != a.cfg.RunEpoch {
		return errors.New("equipment run mismatch")
	}
	a.sample = sample
	a.haveSample = true
	if a.active != nil {
		c := a.active.Command
		if a.active.Stage == "accepted" && sample.AppliedSeq == c.Sequence {
			holding, err := a.modbus.ReadRegisters(0, protocol.FrameCount, modbus.HOLDING_REGISTER)
			if err != nil {
				return err
			}
			frame := a.active.Frame
			if !equalRegisters(frame[:], holding) {
				return errors.New("ambiguous equipment readback")
			}
			if err = a.state.event(c, "applied", "", sample.Elapsed, nil); err != nil {
				return err
			}
			a.active.Stage = "applied"
			if err = a.report(ctx); err != nil {
				return err
			}
			slog.Info("command applied", "command_id", c.ID, "correlation_id", c.CorrelationID)
		} else if a.active.Stage == "accepted" && !now.Before(c.ExpiresAt) {
			if err = a.state.event(c, "rejected", "expired_command", sample.Elapsed, nil); err != nil {
				return err
			}
			a.active = nil
		} else if c.Generation == a.config.Generation && sample.ObservedAt.Before(c.ValidUntil) &&
			!sample.ObservedAt.Before(c.ValidFrom) && now.Sub(a.lastRenewal) >= 2*time.Second {
			leased := c.Parameters.Mode == "baseline" ||
				(a.lease.Sequence == c.Sequence && a.lease.Generation == c.Generation && now.Before(a.leaseDeadline))
			if leased {
				frame := a.active.Frame
				if err = a.modbus.WriteRegisters(0, frame[:]); err != nil {
					return err
				}
				a.lastRenewal = now
			}
		}
	}
	if !a.sampled || sample.Elapsed >= a.lastSample+60 || now.Sub(a.lastSampleWall) >= 5*time.Second {
		a.sampleCounter++
		sample.Sequence = a.sampleCounter
		batch := protocol.Batch{
			SchemaVersion: 1,
			DeviceID:      a.cfg.DeviceID,
			RunID:         a.cfg.RunID,
			BootID:        a.boot,
			ID:            uuid.NewString(),
			Origin:        "simulation",
			SentAt:        now.UTC(),
			Samples:       []protocol.Sample{sample},
		}
		if err = a.state.batch(batch); err != nil {
			return err
		}
		a.lastSample = sample.Elapsed
		a.sampled = true
		a.lastSampleWall = now
		// A fresh snapshot goes first, even when the durable history queue is draining.
		if a.mqtt.Ready.Load() {
			if err = a.mqtt.JSON(
				ctx,
				protocol.Topic(a.identity.Organization, a.cfg.DeviceID, "telemetry"),
				batch,
				false,
			); err != nil {
				slog.Warn("live sample queued", "error", err)
			}
		}
	}
	return nil
}

func equalRegisters(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (a *agent) flush(ctx context.Context) error {
	if !a.mqtt.Ready.Load() {
		return nil
	}
	for _, kind := range []struct{ bucket, suffix string }{{"events", "command/event"}, {"batches", "telemetry"}} {
		pending, err := a.state.pending(kind.bucket, 10)
		if err != nil {
			return err
		}
		for _, m := range pending {
			if err = a.mqtt.Publish(
				ctx,
				protocol.Topic(a.identity.Organization, a.cfg.DeviceID, kind.suffix),
				m.data,
				false,
			); err != nil {
				return err
			}
		}
	}
	return nil
}
