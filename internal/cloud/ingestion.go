package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
)

func (a *App) ingest(ctx context.Context, msg messaging.Message) error {
	parts := strings.Split(msg.Topic, "/")
	if len(parts) < 7 {
		return errors.New("invalid MQTT topic")
	}
	_, err := a.manufacturerRoute(ctx, parts[5])
	if err == nil {
		return nil
	} // Manufacturer devices must cross the authenticated HTTPS boundary.
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return a.ingestDevice(ctx, msg)
}

func (a *App) ingestDevice(ctx context.Context, msg messaging.Message) error {
	parts := strings.Split(msg.Topic, "/")
	if len(parts) < 7 || parts[0] != "thermora" || parts[1] != "v1" || parts[2] != "org" || parts[4] != "device" {
		return errors.New("invalid MQTT topic")
	}
	org, device, suffix := parts[3], parts[5], strings.Join(parts[6:], "/")
	for _, id := range []string{org, device} {
		if _, err := uuid.Parse(id); err != nil {
			return errors.New("invalid topic identity")
		}
	}
	switch suffix {
	case "demo/event":
		return a.ingestRun(ctx, org, device, msg.Payload)
	case "telemetry":
		return a.ingestTelemetry(ctx, org, device, msg.Payload)
	case "command/event":
		return a.ingestEvent(ctx, org, device, msg.Payload)
	case "config/reported":
		var r protocol.ConfigReport
		if err := protocol.Decode(msg.Payload, &r); err != nil {
			return err
		}
		if r.SchemaVersion != 1 || r.DeviceID != device || (r.Status != "accepted" && r.Status != "applied") ||
			r.Accepted == 0 || r.Desired != r.Accepted || r.Reported > r.Accepted ||
			r.ReportedAt.IsZero() || r.ReportedAt.After(time.Now().Add(5*time.Second)) {
			return errors.New("invalid configuration report")
		}
		_, err := a.DB.Exec(
			ctx,
			`UPDATE devices SET accepted_version=GREATEST(accepted_version,$4),reported_version=GREATEST(reported_version,$5) WHERE id=$1 AND organization_id=$2 AND run_id=$3 AND enrollment='active' AND (config->>'config_version')::bigint=$4`,
			device,
			org,
			r.RunID,
			r.Accepted,
			r.Reported,
		)
		return err
	case "hello":
		var hello protocol.Hello
		if err := protocol.Decode(msg.Payload, &hello); err != nil {
			return err
		}
		if hello.DeviceID != device {
			return errors.New("invalid hello identity")
		}
		if _, err := uuid.Parse(hello.BootID); err != nil {
			return errors.New("invalid hello boot")
		}
		if hello.Diagnostics != nil {
			v := hello.Diagnostics
			if v.RecordedAt.IsZero() || v.RecordedAt.After(time.Now().Add(5*time.Second)) || v.BufferedBatches < 0 ||
				v.BufferedBatches > 1000000 ||
				v.BufferedBytes < 0 ||
				v.BufferedBytes > 128*1024*1024 ||
				v.PendingEvents < 0 ||
				v.PendingEvents > 10000 ||
				v.JournalEntries < 0 ||
				v.JournalEntries > 10000 {
				return errors.New("invalid diagnostics")
			}
			if _, err := a.DB.Exec(
				ctx,
				`UPDATE devices SET diagnostics=$4 WHERE id=$1 AND organization_id=$2 AND run_id=$3 AND enrollment='active' AND (diagnostics IS NULL OR (diagnostics->>'recorded_at_wall')::timestamptz<$5)`,
				device,
				org,
				hello.RunID,
				v,
				v.RecordedAt,
			); err != nil {
				return err
			}
		}
		d, err := loadDevice(ctx, a.DB, device, false)
		if err != nil {
			return err
		}
		if d.Org != org || d.Enrollment != "active" {
			return errors.New("inactive device")
		}
		var pending bool
		if err = a.DB.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM run_operations WHERE device_id=$1 AND completed_at IS NULL)`,
			device,
		).Scan(
			&pending,
		); err != nil {
			return err
		}
		if pending {
			return nil
		}
		return a.sendJSON(ctx, protocol.Topic(org, device, "config/desired"), d.Config, true)
	case "heartbeat", "presence":
		return nil // Advisory only: telemetry establishes control freshness.
	default:
		return errors.New("unsupported MQTT message")
	}
}

func (a *App) ingestTelemetry(ctx context.Context, org, device string, data []byte) error {
	receipt, err := a.persistTelemetry(ctx, org, device, data)
	if err != nil {
		return err
	}
	return a.sendJSON(ctx, protocol.Topic(org, device, "telemetry/receipt"), receipt, false)
}

func rejectedReceipt(id, code string) protocol.Receipt {
	return protocol.Receipt{SchemaVersion: 1, BatchID: id, Status: "rejected", Code: code, ReceivedAt: time.Now().UTC()}
}

func (a *App) persistTelemetry(ctx context.Context, org, device string, data []byte) (protocol.Receipt, error) {
	var b protocol.Batch
	if err := protocol.Decode(data, &b); err != nil {
		return protocol.Receipt{}, err
	}
	if _, err := uuid.Parse(b.ID); err != nil {
		return protocol.Receipt{}, err
	}
	if b.DeviceID != device {
		return protocol.Receipt{}, errors.New("topic identity mismatch")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return protocol.Receipt{}, err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, device, true)
	if err != nil {
		return protocol.Receipt{}, err
	}
	allowed, err := connected(ctx, tx, device)
	if err != nil {
		return protocol.Receipt{}, err
	}
	if !allowed || d.Org != org || d.Enrollment != "active" {
		return protocol.Receipt{}, errors.New("inactive device or run")
	}
	currentRun := b.RunID == d.Run
	if !currentRun {
		var known bool
		if err = tx.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM simulation_runs WHERE id=$1 AND device_id=$2 AND finished_at IS NOT NULL AND epoch>=$3)`,
			b.RunID,
			device,
			max(1, int64(d.Epoch)-20),
		).Scan(
			&known,
		); err != nil {
			return protocol.Receipt{}, err
		}
		if !known {
			return rejectedReceipt(b.ID, "unknown_run"), nil
		}
	}
	if err := b.Validate(); err != nil {
		return rejectedReceipt(b.ID, err.Error()), nil
	}
	digest := identity.Digest(string(data))
	var previous []byte
	err = tx.QueryRow(
		ctx,
		`SELECT digest FROM telemetry_batches WHERE organization_id=$1 AND device_id=$2 AND batch_id=$3`,
		org,
		device,
		b.ID,
	).Scan(
		&previous,
	)
	if err == nil {
		if !bytes.Equal(previous, digest) {
			return rejectedReceipt(b.ID, "batch_conflict"), nil
		}
		return protocol.Receipt{SchemaVersion: 1, BatchID: b.ID, Status: "committed", ReceivedAt: time.Now().UTC()}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return protocol.Receipt{}, err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO telemetry_batches(organization_id,device_id,batch_id,digest) VALUES($1,$2,$3,$4)`,
		org,
		device,
		b.ID,
		digest,
	); err != nil {
		return protocol.Receipt{}, err
	}
	now := time.Now().UTC()
	for _, s := range b.Samples {
		if s.RecordedAt.Before(now.Add(-7 * 24 * time.Hour)) {
			return rejectedReceipt(b.ID, "outside_retention"), nil
		}
		if s.ConfigVersion > d.Config.Version || s.RecordedAt.After(now.Add(5*time.Second)) {
			return rejectedReceipt(b.ID, "invalid_sample_context"), nil
		}
		sampleData, err := json.Marshal(s)
		if err != nil {
			return protocol.Receipt{}, err
		}
		sampleDigest := identity.Digest(string(sampleData))
		tag, err := tx.Exec(
			ctx,
			`INSERT INTO telemetry(organization_id,device_id,run_id,boot_id,sample_seq,elapsed,observed_at,sample,digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
			org,
			device,
			b.RunID,
			b.BootID,
			s.Sequence,
			s.Elapsed,
			s.ObservedAt,
			sampleData,
			sampleDigest,
		)
		if err != nil {
			return protocol.Receipt{}, err
		}
		if tag.RowsAffected() == 0 {
			var old []byte
			if err = tx.QueryRow(
				ctx,
				`SELECT digest FROM telemetry WHERE organization_id=$1 AND device_id=$2 AND run_id=$3 AND boot_id=$4 AND sample_seq=$5`,
				org,
				device,
				b.RunID,
				b.BootID,
				s.Sequence,
			).Scan(
				&old,
			); err != nil {
				return protocol.Receipt{}, err
			}
			if !bytes.Equal(old, sampleDigest) {
				return rejectedReceipt(b.ID, "sample_conflict"), nil
			}
		}
		live := now.Sub(s.RecordedAt) <= 10*time.Second && !s.RecordedAt.After(now.Add(5*time.Second)) &&
			len(s.Quality) == 0
		if currentRun && live &&
			(d.Latest == nil || s.Elapsed > d.Latest.Elapsed ||
				(s.Elapsed == d.Latest.Elapsed && s.RecordedAt.After(d.Latest.RecordedAt))) {
			if d.Latest != nil && s.EnergyWh < d.Latest.EnergyWh {
				return rejectedReceipt(b.ID, "counter_reset"), nil
			}
			if _, err = tx.Exec(
				ctx,
				`UPDATE devices SET latest=$2,latest_elapsed=$3,last_received_at=$4,reported_version=GREATEST(reported_version,$5) WHERE id=$1`,
				device,
				sampleData,
				s.Elapsed,
				now,
				s.ConfigVersion,
			); err != nil {
				return protocol.Receipt{}, err
			}
			d.Latest = &s
			d.Received = &now
			if err = confirm(ctx, tx, d, s); err != nil {
				return protocol.Receipt{}, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return protocol.Receipt{}, err
	}
	return protocol.Receipt{SchemaVersion: 1, BatchID: b.ID, Status: "committed", ReceivedAt: now}, nil
}

func confirm(ctx context.Context, tx pgx.Tx, d deviceRecord, s protocol.Sample) error {
	var data []byte
	var stage string
	err := tx.QueryRow(
		ctx,
		`SELECT payload,stage FROM commands WHERE organization_id=$1 AND device_id=$2 AND (payload->>'command_seq')::bigint=$3 AND payload->>'run_id'=$4 ORDER BY created_at DESC LIMIT 1`,
		d.Org,
		d.ID,
		s.AppliedSeq,
		d.Run,
	).Scan(
		&data,
		&stage,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var c protocol.Command
	if err = json.Unmarshal(data, &c); err != nil {
		return err
	}
	if stage == "confirmed" || c.ConfigVersion != s.ConfigVersion || c.Parameters.Mode != s.Mode ||
		s.ObservedAt.Before(c.ValidFrom) ||
		(c.Parameters.Mode == "remote" &&
			(math.Abs(c.Parameters.Room-s.RoomTarget) > .011 ||
				math.Abs(c.Parameters.Supply-s.SupplyTarget) > .011)) {
		return nil
	}
	e := protocol.Event{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		CommandID:     c.ID,
		DeviceID:      d.ID,
		RunID:         d.Run,
		Stage:         "confirmed",
		OccurredAt:    time.Now().UTC(),
		ObservedAt:    s.ObservedAt,
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO command_events(id,organization_id,command_id,payload) VALUES($1,$2,$3,$4)`,
		e.ID,
		d.Org,
		c.ID,
		e,
	); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE commands SET stage='confirmed',stage_rank=5 WHERE id=$1`, c.ID)
	return err
}

var ranks = map[string]int{"received": 2, "accepted": 3, "applied": 4, "rejected": 6}

func (a *App) ingestEvent(ctx context.Context, org, device string, data []byte) error {
	var e protocol.Event
	if err := protocol.Decode(data, &e); err != nil {
		return err
	}
	if _, err := uuid.Parse(e.ID); err != nil {
		return err
	}
	if _, err := uuid.Parse(e.CommandID); err != nil {
		return err
	}
	rank, ok := ranks[e.Stage]
	if !ok || e.SchemaVersion != 1 || e.DeviceID != device {
		return errors.New("invalid command evidence")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, device, true)
	if err != nil {
		return err
	}
	if d.Org != org || d.Enrollment != "active" {
		return errors.New("inactive device or run")
	}
	var commandData []byte
	if err = tx.QueryRow(
		ctx,
		`SELECT payload FROM commands WHERE id=$1 AND organization_id=$2 AND device_id=$3 FOR UPDATE`,
		e.CommandID,
		org,
		device,
	).Scan(
		&commandData,
	); err != nil {
		return err
	}
	var c protocol.Command
	if err = json.Unmarshal(commandData, &c); err != nil {
		return err
	}
	if c.RunID != e.RunID || e.OccurredAt.After(time.Now().Add(5*time.Second)) ||
		e.OccurredAt.Before(c.IssuedAt.Add(-5*time.Second)) {
		return errors.New("invalid command event time")
	}
	tag, err := tx.Exec(
		ctx,
		`INSERT INTO command_events(id,organization_id,command_id,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		e.ID,
		org,
		c.ID,
		e,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var same bool
		if err = tx.QueryRow(
			ctx,
			`SELECT payload=$2::jsonb FROM command_events WHERE id=$1 AND organization_id=$3 AND command_id=$4`,
			e.ID,
			e,
			org,
			c.ID,
		).Scan(
			&same,
		); err != nil ||
			!same {
			return fmt.Errorf("event content conflict")
		}
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE commands SET stage=$2,stage_rank=$3 WHERE id=$1 AND stage_rank<$3 AND stage NOT IN ('confirmed','rejected')`,
		c.ID,
		e.Stage,
		rank,
	); err != nil {
		return err
	}
	if e.Stage != "received" {
		if _, err = tx.Exec(ctx, `DELETE FROM outbox WHERE command_id=$1`, c.ID); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if err = a.sendJSON(
		ctx,
		protocol.Topic(org, device, "command/event/receipt"),
		protocol.Receipt{SchemaVersion: 1, EventID: e.ID, Status: "committed", ReceivedAt: time.Now().UTC()},
		false,
	); err != nil {
		return err
	}
	if e.Stage == "accepted" {
		return a.lease(ctx, d, c)
	}
	return nil
}
