package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/reporting"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func (a *App) operator(ctx context.Context, org string) error {
	var allowed bool
	if err := a.DB.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM memberships WHERE organization_id=$1 AND user_id=$2 AND role='operator')`,
		org,
		request(ctx).userID,
	).Scan(
		&allowed,
	); err != nil {
		return err
	}
	if !allowed {
		return missing()
	}
	return nil
}

// flexibilityEligibility is shared by the operator preview and the sole control resolver.
func flexibilityEligibility(ctx context.Context, q queryer, d deviceRecord) (string, error) {
	allowed, err := connected(ctx, q, d.ID)
	if err != nil {
		return "", err
	}
	if !allowed || d.Enrollment != "active" {
		return "disconnected", nil
	}
	if !d.Config.Automation.Flexibility {
		return "owner_not_opted_in", nil
	}
	if !d.Config.Automation.Optimization {
		return "automation_disabled", nil
	}
	if d.OverrideTarget != nil {
		return "owner_override", nil
	}
	if d.Latest == nil || d.Received == nil || time.Since(*d.Received) > 15*time.Second {
		return "stale_telemetry", nil
	}
	if d.Reported != d.Config.Version {
		return "configuration_pending", nil
	}
	if len(d.Latest.Alarms) > 0 || len(d.Latest.Quality) > 0 {
		return "equipment_or_data_quality", nil
	}
	if d.Latest.Indoor <= d.Config.Comfort.Min+.25 {
		return "comfort_protection", nil
	}
	if d.Latest.HotWater != nil && d.Latest.HotWater.Heating {
		return "hot_water_priority", nil
	}
	var blocked bool
	if err = q.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM simulation_runs WHERE id=$1 AND paused) OR EXISTS(SELECT 1 FROM run_operations WHERE device_id=$2 AND completed_at IS NULL)`,
		d.Run,
		d.ID,
	).Scan(
		&blocked,
	); err != nil {
		return "", err
	}
	if blocked {
		return "run_paused_or_changing", nil
	}
	return "eligible", nil
}

func (a *App) fleetIDs(ctx context.Context, org string) ([]string, error) {
	rows, err := a.DB.Query(
		ctx,
		`SELECT id FROM devices WHERE organization_id=$1 AND building_id IS NOT NULL ORDER BY id LIMIT 25`,
		org,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (a *App) GetFleet(ctx context.Context, r api.GetFleetRequestObject) (api.GetFleetResponseObject, error) {
	if err := a.operator(ctx, r.OrganizationId); err != nil {
		return nil, err
	}
	ids, err := a.fleetIDs(ctx, r.OrganizationId)
	if err != nil {
		return nil, err
	}
	result := []api.FleetDevice{}
	// ponytail: bounded 25-device local fleet; use one joined read if this grows beyond demo scale.
	for _, id := range ids {
		d, err := loadDevice(ctx, a.DB, id, false)
		if err != nil {
			return nil, err
		}
		reason, err := flexibilityEligibility(ctx, a.DB, d)
		if err != nil {
			return nil, err
		}
		item := api.FleetDevice{Device: deviceView(d), Eligibility: reason}
		if err = a.DB.QueryRow(
			ctx,
			`SELECT name FROM buildings WHERE id=$1 AND organization_id=$2`,
			d.Building,
			r.OrganizationId,
		).Scan(
			&item.BuildingName,
		); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return api.GetFleet200JSONResponse(result), nil
}

func fenceFlexibility(ctx context.Context, tx pgx.Tx, d *deviceRecord) error {
	d.Config.Generation++
	d.Config.IssuedAt = time.Now().UTC()
	if err := fenceDevice(ctx, tx, *d); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET config=$2 WHERE id=$1`, d.ID, d.Config); err != nil {
		return err
	}
	return queueConfig(ctx, tx, *d)
}

// flexIntent terminates an individual intention on any lost eligibility. It cannot
// silently rejoin the same event when an owner cancels an override or opts back in.
func flexIntent(ctx context.Context, tx pgx.Tx, d *deviceRecord) (*time.Time, error) {
	var event, run, status string
	var until, wall time.Time
	err := tx.QueryRow(
		ctx,
		`SELECT e.id,p.run_id,p.end_sim,e.expires_at,e.status FROM flexibility_participants p JOIN flexibility_events e ON e.id=p.event_id WHERE p.device_id=$1 AND p.status='eligible' ORDER BY e.created_at DESC LIMIT 1`,
		d.ID,
	).Scan(
		&event,
		&run,
		&until,
		&wall,
		&status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	reason, err := flexibilityEligibility(ctx, tx, *d)
	if err != nil {
		return nil, err
	}
	state := "excluded"
	switch {
	case status != "active":
		reason = "event_" + status
	case time.Now().After(wall):
		reason = "wall_time_limit"
	case run != d.Run:
		reason = "run_changed"
	case d.Latest != nil && !d.Latest.ObservedAt.Before(until):
		reason = "window_complete"
		state = "completed"
	}
	if reason == "eligible" {
		return &until, nil
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE flexibility_participants SET status=$3,reason=$4 WHERE event_id=$1 AND device_id=$2`,
		event,
		d.ID,
		state,
		reason,
	); err != nil {
		return nil, err
	}
	return nil, fenceFlexibility(ctx, tx, d)
}

func (a *App) CreateFlexibilityEvent(
	ctx context.Context,
	r api.CreateFlexibilityEventRequestObject,
) (api.CreateFlexibilityEventResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if err := a.operator(ctx, r.OrganizationId); err != nil {
		return nil, err
	}
	ids, err := a.fleetIDs(ctx, r.OrganizationId)
	if err != nil {
		return nil, err
	}
	s := request(ctx)
	event, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"flexibility:"+r.OrganizationId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.FlexibilityEvent, error) {
			result := api.FlexibilityEvent{
				Id:                 uuid.NewString(),
				OrganizationId:     r.OrganizationId,
				Status:             "active",
				CreatedAtWall:      time.Now().UTC(),
				ExpiresAtWall:      time.Now().UTC().Add(2 * time.Hour),
				DurationSimMinutes: r.Body.DurationSimMinutes,
				Participants:       []api.FlexParticipant{},
			}
			var active bool
			if err := tx.QueryRow(
				ctx,
				`SELECT EXISTS(SELECT 1 FROM flexibility_events WHERE organization_id=$1 AND status='active')`,
				r.OrganizationId,
			).Scan(
				&active,
			); err != nil {
				return result, err
			}
			if active {
				return result, fail(409, "event_active", "Complete or cancel the current event first.")
			}
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO flexibility_events(id,organization_id,status,expires_at,duration_minutes) VALUES($1,$2,'active',$3,$4)`,
				result.Id,
				r.OrganizationId,
				result.ExpiresAtWall,
				result.DurationSimMinutes,
			); err != nil {
				return result, err
			}
			eligible := 0
			for _, id := range ids {
				d, err := loadDevice(ctx, tx, id, true)
				if err != nil {
					return result, err
				}
				reason, err := flexibilityEligibility(ctx, tx, d)
				if err != nil {
					return result, err
				}
				p := api.FlexParticipant{
					DeviceId: id,
					RunId:    d.Run,
					Start:    protocol.StartTime,
					Status:   "excluded",
					Reason:   reason,
				}
				p.CommandCounts = make([]struct {
					Count int    `json:"count"`
					Stage string `json:"stage"`
				}, 0)
				if d.Latest != nil {
					p.Start = d.Latest.ObservedAt
					p.ReferencePowerKw = new(float64(d.Latest.ElectricalW) / 1000)
				}
				p.End = p.Start.Add(time.Duration(result.DurationSimMinutes) * time.Minute)
				if reason == "eligible" {
					p.Status = "eligible"
					eligible++
					if err = fenceFlexibility(ctx, tx, &d); err != nil {
						return result, err
					}
				}
				if _, err = tx.Exec(
					ctx,
					`INSERT INTO flexibility_participants(event_id,organization_id,device_id,run_id,start_sim,end_sim,status,reason,reference_power_kw) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
					result.Id,
					d.Org,
					id,
					d.Run,
					p.Start,
					p.End,
					p.Status,
					p.Reason,
					p.ReferencePowerKw,
				); err != nil {
					return result, err
				}
				result.Participants = append(result.Participants, p)
			}
			if eligible == 0 {
				return result, fail(
					409,
					"no_eligible_devices",
					"No devices are currently eligible; review owner consent, automation and freshness.",
				)
			}
			return result, audit(ctx, tx, r.OrganizationId, s.userID, "flexibility_created", result.Id, s.id)
		},
	)
	return api.CreateFlexibilityEvent201JSONResponse(event), err
}

func (a *App) flexibilityEvent(ctx context.Context, org, id string, evidence bool) (api.FlexibilityEvent, error) {
	var event api.FlexibilityEvent
	err := a.DB.QueryRow(
		ctx,
		`SELECT id,organization_id,status,created_at,expires_at,duration_minutes FROM flexibility_events WHERE id=$1 AND organization_id=$2`,
		id,
		org,
	).Scan(
		&event.Id,
		&event.OrganizationId,
		&event.Status,
		&event.CreatedAtWall,
		&event.ExpiresAtWall,
		&event.DurationSimMinutes,
	)
	if err != nil {
		return event, err
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT device_id,run_id,start_sim,end_sim,status,reason,reference_power_kw FROM flexibility_participants WHERE event_id=$1 AND organization_id=$2 ORDER BY device_id`,
		id,
		org,
	)
	if err != nil {
		return event, err
	}
	event.Participants = []api.FlexParticipant{}
	for rows.Next() {
		var p api.FlexParticipant
		if err = rows.Scan(
			&p.DeviceId,
			&p.RunId,
			&p.Start,
			&p.End,
			&p.Status,
			&p.Reason,
			&p.ReferencePowerKw,
		); err != nil {
			rows.Close()
			return event, err
		}
		p.CommandCounts = make([]struct {
			Count int    `json:"count"`
			Stage string `json:"stage"`
		}, 0)
		event.Participants = append(event.Participants, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return event, err
	}
	if evidence {
		for i := range event.Participants {
			p := &event.Participants[i]
			var counts []byte
			if err = a.DB.QueryRow(
				ctx,
				`SELECT COALESCE(jsonb_agg(jsonb_build_object('stage',stage,'count',n)),'[]') FROM (SELECT stage,count(*) n FROM commands WHERE flexibility_event_id=$1 AND device_id=$2 GROUP BY stage ORDER BY stage) states`,
				id,
				p.DeviceId,
			).Scan(
				&counts,
			); err != nil {
				return event, err
			}
			if err = json.Unmarshal(counts, &p.CommandCounts); err != nil {
				return event, err
			}
			if p.Status == "excluded" {
				continue
			}
			d, err := loadDevice(ctx, a.DB, p.DeviceId, false)
			if err != nil {
				return event, err
			}
			readings, err := historyReadings(ctx, a.DB, d, p.RunId, p.Start, p.End)
			if err != nil {
				return event, err
			}
			h, err := reporting.Summarize(readings, p.Start, p.End, 15)
			if err != nil {
				return event, err
			}
			p.CoveredSeconds = h.CoveredSeconds
			p.CoveredEnergyKwh = new(h.Energy)
			if h.GapSeconds == 0 && p.ReferencePowerKw != nil {
				p.ObservedMeanKw = new(h.Energy / p.End.Sub(p.Start).Hours())
				p.ObservedChangeKw = new(*p.ReferencePowerKw - *p.ObservedMeanKw)
			}
		}
	}
	return event, nil
}

func (a *App) GetFlexibilityEvent(
	ctx context.Context,
	r api.GetFlexibilityEventRequestObject,
) (api.GetFlexibilityEventResponseObject, error) {
	if err := a.operator(ctx, r.OrganizationId); err != nil {
		return nil, err
	}
	event, err := a.flexibilityEvent(ctx, r.OrganizationId, r.EventId, true)
	return api.GetFlexibilityEvent200JSONResponse(event), err
}

func (a *App) ListFlexibilityEvents(
	ctx context.Context,
	r api.ListFlexibilityEventsRequestObject,
) (api.ListFlexibilityEventsResponseObject, error) {
	if err := a.operator(ctx, r.OrganizationId); err != nil {
		return nil, err
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT id FROM flexibility_events WHERE organization_id=$1 ORDER BY created_at DESC LIMIT 10`,
		r.OrganizationId,
	)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	result := []api.FlexibilityEvent{}
	for _, id := range ids {
		event, err := a.flexibilityEvent(ctx, r.OrganizationId, id, false)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return api.ListFlexibilityEvents200JSONResponse(result), nil
}

func (a *App) CancelFlexibilityEvent(
	ctx context.Context,
	r api.CancelFlexibilityEventRequestObject,
) (api.CancelFlexibilityEventResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if err := a.operator(ctx, r.OrganizationId); err != nil {
		return nil, err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	var status string
	if err = tx.QueryRow(
		ctx,
		`SELECT status FROM flexibility_events WHERE id=$1 AND organization_id=$2 FOR UPDATE`,
		r.EventId,
		r.OrganizationId,
	).Scan(
		&status,
	); err != nil {
		return nil, err
	}
	if status != "active" {
		return api.CancelFlexibilityEvent204Response{}, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE flexibility_events SET status='cancelled' WHERE id=$1`, r.EventId); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		ctx,
		`SELECT device_id FROM flexibility_participants WHERE event_id=$1 AND status='eligible' ORDER BY device_id`,
		r.EventId,
	)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		d, err := loadDevice(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		if _, err = flexIntent(ctx, tx, &d); err != nil {
			return nil, err
		}
	}
	if err = audit(
		ctx,
		tx,
		r.OrganizationId,
		request(ctx).userID,
		"flexibility_cancelled",
		r.EventId,
		request(ctx).id,
	); err != nil {
		return nil, err
	}
	return api.CancelFlexibilityEvent204Response{}, tx.Commit(ctx)
}

func (a *App) finishFlexibility(ctx context.Context) error {
	_, err := a.DB.Exec(
		ctx,
		`UPDATE flexibility_events e SET status=CASE WHEN expires_at<=now() THEN 'timed_out' ELSE 'completed' END WHERE status='active' AND (expires_at<=now() OR NOT EXISTS(SELECT 1 FROM flexibility_participants p WHERE p.event_id=e.id AND p.status='eligible'))`,
	)
	return err
}

func excludeFlexibility(ctx context.Context, tx pgx.Tx, device, reason string) error {
	_, err := tx.Exec(
		ctx,
		`UPDATE flexibility_participants SET status='excluded',reason=$2 WHERE device_id=$1 AND status='eligible'`,
		device,
		reason,
	)
	return err
}
