package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/sim"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func queueConfig(ctx context.Context, tx pgx.Tx, d deviceRecord) error {
	t, err := tariff(ctx, tx, d.Building)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO device_configurations(device_id,version,configuration,tariff) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		d.ID,
		d.Config.Version,
		d.Config,
		t,
	); err != nil {
		return err
	}

	_, err = tx.Exec(
		ctx,
		`INSERT INTO outbox(organization_id,device_id,topic,payload,retained) VALUES($1,$2,$3,$4,true)`,
		d.Org,
		d.ID,
		protocol.Topic(d.Org, d.ID, "config/desired"),
		d.Config,
	)
	return err
}

// issue is the sole cloud command writer. Callers hold the device row lock.
func issue(
	ctx context.Context,
	tx pgx.Tx,
	d deviceRecord,
	mode, reason string,
	target float64,
	until time.Time,
	correlation string,
) (protocol.Command, error) {
	var c protocol.Command
	if d.Latest == nil || d.Sequence == ^uint32(0) {
		return c, fail(409, "device_not_ready", "Wait for current device telemetry.")
	}
	now := time.Now().UTC()
	c = protocol.Command{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      d.ID,
		RunID:         d.Run,
		RunEpoch:      d.Epoch,
		Sequence:      d.Sequence + 1,
		Generation:    d.Config.Generation,
		ConfigVersion: d.Config.Version,
		IssuedAt:      now,
		ExpiresAt:     now.Add(30 * time.Second),
		ValidFrom:     d.Latest.ObservedAt,
		ValidUntil:    until,
		Kind:          "set_heating_policy",
		Source:        "baseline",
		Reason:        reason,
		CorrelationID: correlation,
		Parameters: protocol.Parameters{
			Mode:   mode,
			Room:   target,
			Supply: sim.BaselineSupply(d.Latest.Indoor, d.Latest.Outdoor, target),
		},
	}
	if mode == "remote" {
		c.Source = "manual_override"
		if reason != "manual_override" {
			c.Source = "price_optimization"
			if reason == "flexibility_dispatch" {
				c.Source = "flexibility_dispatch"
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET command_seq=$2 WHERE id=$1`, d.ID, c.Sequence); err != nil {
		return c, err
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO commands(id,organization_id,device_id,payload,configuration) VALUES($1,$2,$3,$4,$5)`,
		c.ID,
		d.Org,
		d.ID,
		c,
		d.Config,
	); err != nil {
		return c, err
	}
	if c.Source == "flexibility_dispatch" {
		if _, err := tx.Exec(
			ctx,
			`UPDATE commands SET flexibility_event_id=(SELECT p.event_id FROM flexibility_participants p JOIN flexibility_events e ON e.id=p.event_id WHERE p.device_id=$2 AND p.run_id=$3 AND p.status='eligible' AND e.status='active' ORDER BY e.created_at DESC LIMIT 1) WHERE id=$1`,
			c.ID,
			d.ID,
			d.Run,
		); err != nil {
			return c, err
		}
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO outbox(organization_id,device_id,topic,payload,command_id,expires_at) VALUES($1,$2,$3,$4,$5,$6)`,
		d.Org,
		d.ID,
		protocol.Topic(d.Org, d.ID, "command"),
		c,
		c.ID,
		c.ExpiresAt,
	); err != nil {
		return c, err
	}
	return c, nil
}

func (a *App) CreateOverride(
	ctx context.Context,
	r api.CreateOverrideRequestObject,
) (api.CreateOverrideResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	view, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"override:"+r.DeviceId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.Command, error) {
			d, err := loadDevice(ctx, tx, r.DeviceId, true)
			if err != nil {
				return api.Command{}, err
			}
			if d.Enrollment != "active" || d.Received == nil || time.Since(*d.Received) > 15*time.Second ||
				d.Latest == nil ||
				d.Reported != d.Config.Version {
				return api.Command{}, fail(
					409,
					"device_not_ready",
					"Wait for fresh telemetry and applied configuration before overriding.",
				)
			}
			allowed, err := connected(ctx, tx, d.ID)
			if err != nil {
				return api.Command{}, err
			}
			if !allowed {
				return api.Command{}, fail(
					409,
					"device_disconnected",
					"Reconnect the device before requesting control.",
				)
			}
			target := float64(r.Body.RoomTargetC)
			if target < d.Config.Comfort.Min || target > d.Config.Comfort.Max {
				return api.Command{}, fail(422, "outside_comfort", "The override must stay inside the comfort range.")
			}
			if err = excludeFlexibility(ctx, tx, d.ID, "owner_override"); err != nil {
				return api.Command{}, err
			}
			until := d.Latest.ObservedAt.Add(time.Duration(r.Body.DurationSimMinutes) * time.Minute)
			d.Config.Generation++
			d.Config.IssuedAt = time.Now().UTC()
			if _, err = tx.Exec(
				ctx,
				`UPDATE devices SET config=$2,override_target=$3,override_until=$4 WHERE id=$1`,
				d.ID,
				d.Config,
				target,
				until,
			); err != nil {
				return api.Command{}, err
			}
			if err = queueConfig(ctx, tx, d); err != nil {
				return api.Command{}, err
			}
			c, err := issue(ctx, tx, d, "remote", "manual_override", target, until, s.id)
			if err != nil {
				return api.Command{}, err
			}
			if err = audit(ctx, tx, d.Org, s.userID, "override_created", c.ID, s.id); err != nil {
				return api.Command{}, err
			}
			return commandView(c, "persisted", nil), nil
		},
	)
	return api.CreateOverride202JSONResponse(view), err
}

func (a *App) CancelOverride(
	ctx context.Context,
	r api.CancelOverrideRequestObject,
) (api.CancelOverrideResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, r.DeviceId, true)
	if err != nil {
		return nil, err
	}
	if d.OverrideTarget == nil {
		return api.CancelOverride204Response{}, nil
	}
	d.Config.Generation++
	d.Config.IssuedAt = time.Now().UTC()
	if _, err = tx.Exec(
		ctx,
		`UPDATE devices SET config=$2,override_target=NULL,override_until=NULL WHERE id=$1`,
		d.ID,
		d.Config,
	); err != nil {
		return nil, err
	}
	if err = queueConfig(ctx, tx, d); err != nil {
		return nil, err
	}
	if d.Latest != nil {
		if _, err = issue(
			ctx,
			tx,
			d,
			"baseline",
			"override_cancelled",
			d.Config.Comfort.Target,
			d.Latest.ObservedAt.Add(24*time.Hour),
			s.id,
		); err != nil {
			return nil, err
		}
	}
	if err = audit(ctx, tx, d.Org, s.userID, "override_cancelled", d.ID, s.id); err != nil {
		return nil, err
	}
	return api.CancelOverride204Response{}, tx.Commit(ctx)
}

func commandView(c protocol.Command, stage string, events []protocol.Event) api.Command {
	v := api.Command{
		Id:              c.ID,
		Sequence:        int(c.Sequence),
		Stage:           api.CommandStage(stage),
		ReasonCode:      c.Reason,
		RoomTargetC:     c.Parameters.Room,
		SupplySetpointC: c.Parameters.Supply,
		Mode:            c.Parameters.Mode,
		IssuedAtWall:    c.IssuedAt,
		ExpiresAtWall:   c.ExpiresAt,
		ValidUntil:      c.ValidUntil,
	}
	v.Events = make([]struct {
		OccurredAtWall time.Time `json:"occurred_at_wall"`
		ReasonCode     string    `json:"reason_code"`
		Stage          string    `json:"stage"`
	}, 0, len(events))
	for _, e := range events {
		v.Events = append(v.Events, struct {
			OccurredAtWall time.Time `json:"occurred_at_wall"`
			ReasonCode     string    `json:"reason_code"`
			Stage          string    `json:"stage"`
		}{e.OccurredAt, e.Reason, e.Stage})
	}
	return v
}

func (a *App) ListCommands(
	ctx context.Context,
	r api.ListCommandsRequestObject,
) (api.ListCommandsResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	if r.Params.BeforeSequence != nil && r.Params.RunId == nil {
		return nil, fail(422, "run_required", "Select a run before paging command history.")
	}
	limit := 50
	if r.Params.Limit != nil {
		limit = *r.Params.Limit
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT c.payload,c.stage,COALESCE((SELECT jsonb_agg(e.payload ORDER BY e.received_at) FROM command_events e WHERE e.organization_id=c.organization_id AND e.command_id=c.id),'[]') FROM commands c WHERE c.device_id=$1 AND ($2::text IS NULL OR c.payload->>'run_id'=$2) AND ($3::bigint IS NULL OR (c.payload->>'command_seq')::bigint<$3) ORDER BY CASE WHEN $2::text IS NOT NULL THEN (c.payload->>'command_seq')::bigint END DESC,c.created_at DESC,c.id DESC LIMIT $4`,
		r.DeviceId,
		r.Params.RunId,
		r.Params.BeforeSequence,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []api.Command{}
	for rows.Next() {
		var data, eventData []byte
		var stage string
		if err = rows.Scan(&data, &stage, &eventData); err != nil {
			return nil, err
		}
		var c protocol.Command
		var events []protocol.Event
		if err = json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(eventData, &events); err != nil {
			return nil, err
		}
		result = append(result, commandView(c, stage, events))
	}
	return api.ListCommands200JSONResponse(result), rows.Err()
}

func (a *App) resolve(ctx context.Context, id string) error {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, id, true)
	if err != nil {
		return err
	}
	flexUntil, err := flexIntent(ctx, tx, &d)
	if err != nil {
		return err
	}
	allowed, err := connected(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	if !allowed || d.Enrollment != "active" || d.Latest == nil || d.Received == nil ||
		time.Since(*d.Received) > 15*time.Second ||
		d.Accepted != d.Config.Version {
		return tx.Commit(ctx)
	}
	var paused, pending bool
	if err = tx.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM simulation_runs WHERE id=$1 AND paused),EXISTS(SELECT 1 FROM run_operations WHERE device_id=$2 AND completed_at IS NULL)`,
		d.Run,
		d.ID,
	).Scan(
		&paused,
		&pending,
	); err != nil {
		return err
	}
	if paused || pending {
		return tx.Commit(ctx)
	}
	var lastData []byte
	var stage string
	err = tx.QueryRow(
		ctx,
		`SELECT payload,stage FROM commands WHERE device_id=$1 AND payload->>'run_id'=$2 ORDER BY created_at DESC LIMIT 1`,
		id,
		d.Run,
	).Scan(
		&lastData,
		&stage,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var last protocol.Command
	if len(lastData) > 0 {
		if err = json.Unmarshal(lastData, &last); err != nil {
			return err
		}
	}
	if d.OverrideUntil != nil && !d.Latest.ObservedAt.Before(*d.OverrideUntil) {
		d.Config.Generation++
		d.Config.IssuedAt = time.Now().UTC()
		d.OverrideTarget = nil
		d.OverrideUntil = nil
		if _, err = tx.Exec(
			ctx,
			`UPDATE devices SET config=$2,override_target=NULL,override_until=NULL WHERE id=$1`,
			d.ID,
			d.Config,
		); err != nil {
			return err
		}
		if err = queueConfig(ctx, tx, d); err != nil {
			return err
		}
	}
	mode, reason := "baseline", "baseline_restored"
	target, until := d.Config.Comfort.Target, d.Latest.ObservedAt.Add(24*time.Hour)
	if d.Reported != d.Config.Version {
		reason = "config_sync"
	} else if d.OverrideTarget != nil && d.OverrideUntil != nil {
		mode, reason, target, until = "remote", "manual_override", *d.OverrideTarget, *d.OverrideUntil
	} else if flexUntil != nil {
		mode, reason, target, until = "remote", "flexibility_dispatch", d.Config.Comfort.Min+.25, *flexUntil
	} else if d.Config.Automation.Optimization {
		plan, planErr := activePlan(ctx, tx, d)
		if planErr != nil {
			return planErr
		}
		reason = plan.Reason
		for _, interval := range plan.Intervals {
			if !d.Latest.ObservedAt.Before(interval.Start) && d.Latest.ObservedAt.Before(interval.End) {
				until = interval.End
				if plan.Adopted {
					mode, reason, target = "remote", interval.Reason, interval.Room
				}
				break
			}
		}
	}
	needsControl := last.ID == "" || last.Generation != d.Config.Generation || last.ConfigVersion != d.Config.Version ||
		last.Parameters.Mode != mode ||
		last.Reason != reason ||
		(mode == "remote" && last.Parameters.Room != target) ||
		!d.Latest.ObservedAt.Before(last.ValidUntil) ||
		stage == "expired" ||
		stage == "rejected"
	commandPending := last.ID != "" && last.Generation == d.Config.Generation &&
		last.ConfigVersion == d.Config.Version &&
		time.Now().Before(last.ExpiresAt) &&
		d.Latest.ObservedAt.Before(last.ValidUntil) &&
		stage != "rejected" &&
		stage != "expired" &&
		stage != "applied" &&
		stage != "confirmed"
	if needsControl && !commandPending {
		if _, err = issue(ctx, tx, d, mode, reason, target, until, uuid.NewString()); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
