package cloud

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/reporting"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func (a *App) ListRuns(ctx context.Context, r api.ListRunsRequestObject) (api.ListRunsResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	rows, err := a.DB.Query(ctx, `SELECT r.id,r.epoch,r.model,r.created_at,r.finished_at,
 (SELECT observed_at FROM telemetry t WHERE t.organization_id=d.organization_id AND t.device_id=d.id AND t.run_id=r.id ORDER BY elapsed LIMIT 1),
 (SELECT observed_at FROM telemetry t WHERE t.organization_id=d.organization_id AND t.device_id=d.id AND t.run_id=r.id ORDER BY elapsed DESC LIMIT 1)
 FROM simulation_runs r JOIN devices d ON d.id=r.device_id WHERE r.device_id=$1 ORDER BY r.epoch DESC LIMIT 100`, r.DeviceId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []api.RunSummary{}
	for rows.Next() {
		var run api.RunSummary
		if err = rows.Scan(
			&run.RunId,
			&run.Epoch,
			&run.Model,
			&run.CreatedAtWall,
			&run.FinishedAtWall,
			&run.FirstObservedAt,
			&run.LastObservedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return api.ListRuns200JSONResponse(result), rows.Err()
}

func historyReadings(
	ctx context.Context,
	q queryer,
	d deviceRecord,
	run string,
	from, to time.Time,
) ([]reporting.Reading, error) {
	if !to.After(from) || to.Sub(from) > 7*24*time.Hour {
		return nil, fail(422, "invalid_window", "Select a positive window of at most seven days.")
	}
	if run != d.Run {
		var known bool
		if err := q.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM simulation_runs WHERE id=$1 AND device_id=$2)`,
			run,
			d.ID,
		).Scan(
			&known,
		); err != nil {
			return nil, err
		}
		if !known {
			return nil, missing()
		}
	}
	// Neighbour samples allow correct clipping at requested boundaries. Duplicate elapsed
	// instants (including pause heartbeats) contribute energy exactly once.
	rows, err := q.Query(
		ctx,
		`SELECT t.sample,c.configuration,COALESCE(c.tariff,''),COALESCE(cmd.payload->>'source','') FROM
 (SELECT DISTINCT ON(elapsed) sample,elapsed FROM telemetry WHERE organization_id=$1 AND device_id=$2 AND run_id=$3 AND elapsed BETWEEN $4 AND $5 ORDER BY elapsed,(sample->>'recorded_at_wall') DESC,received_at DESC,boot_id,sample_seq DESC LIMIT 20001) t
 LEFT JOIN device_configurations c ON c.device_id=$2 AND c.version=(t.sample->>'reported_config_version')::bigint LEFT JOIN commands cmd ON cmd.device_id=$2 AND cmd.payload->>'run_id'=$3::text AND (cmd.payload->>'command_seq')::bigint=(t.sample->>'applied_command_seq')::bigint AND cmd.stage IN ('applied','confirmed') ORDER BY t.elapsed`,
		d.Org,
		d.ID,
		run,
		int64(from.Sub(protocol.StartTime).Seconds())-180,
		int64(to.Sub(protocol.StartTime).Seconds())+180,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []reporting.Reading{}
	for rows.Next() {
		var sample, config []byte
		var reading reporting.Reading
		if err = rows.Scan(&sample, &config, &reading.Tariff, &reading.Source); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(sample, &reading.Sample); err != nil {
			return nil, err
		}
		if len(config) > 0 {
			if err = json.Unmarshal(config, &reading.Configuration); err != nil {
				return nil, err
			}
		}
		result = append(result, reading)
	}
	if len(result) > 20000 {
		return nil, fail(
			422,
			"window_too_dense",
			"Choose a shorter window; the query is limited to 20,000 unique sample times.",
		)
	}
	return result, rows.Err()
}

func (a *App) GetHistory(ctx context.Context, r api.GetHistoryRequestObject) (api.GetHistoryResponseObject, error) {
	user := request(ctx).userID
	if _, err := a.device(ctx, r.DeviceId, user); err != nil {
		return nil, err
	}
	d, err := loadDevice(ctx, a.DB, r.DeviceId, false)
	if err != nil {
		return nil, err
	}
	b, err := a.building(ctx, d.Building, user)
	if err != nil {
		return nil, err
	}
	run := d.Run
	if r.Params.RunId != nil {
		run = *r.Params.RunId
	}
	var from, to time.Time
	if r.Params.Day != nil {
		if r.Params.From != nil || r.Params.To != nil {
			return nil, fail(422, "invalid_window", "Use a local calendar day or an explicit UTC window, not both.")
		}
		from, to, err = reporting.DayWindow(*r.Params.Day, b.Preferences.Timezone)
	} else if r.Params.From != nil && r.Params.To != nil {
		from, to = *r.Params.From, *r.Params.To
	} else {
		return nil, fail(422, "invalid_window", "Choose a day or both from and to timestamps.")
	}
	if err != nil {
		return nil, fail(422, "invalid_window", "Invalid calendar day or timezone.")
	}
	readings, err := historyReadings(ctx, a.DB, d, run, from, to)
	if err != nil {
		return nil, err
	}
	minutes := 60
	if r.Params.BucketMinutes != nil {
		minutes = int(*r.Params.BucketMinutes)
	}
	history, err := reporting.Summarize(readings, from, to, minutes)
	if err != nil {
		return nil, fail(422, "invalid_window", err.Error())
	}
	history.RunID, history.Timezone = run, b.Preferences.Timezone
	return api.GetHistory200JSONResponse(history), nil
}

func (a *App) CreateReplay(
	ctx context.Context,
	r api.CreateReplayRequestObject,
) (api.CreateReplayResponseObject, error) {
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	if !r.Body.To.After(r.Body.From) || r.Body.To.Sub(r.Body.From) > 24*time.Hour {
		return nil, fail(422, "invalid_window", "Select up to 24 simulated hours.")
	}
	report, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"replay:"+r.DeviceId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (reporting.Replay, error) {
			var result reporting.Replay
			d, err := loadDevice(ctx, tx, r.DeviceId, true)
			if err != nil {
				return result, err
			}
			var count int
			if err = tx.QueryRow(
				ctx,
				`SELECT count(*) FROM observed_replays WHERE device_id=$1`,
				d.ID,
			).Scan(
				&count,
			); err != nil {
				return result, err
			}
			if count >= 100 {
				return result, fail(409, "replay_limit", "This device has reached its 100-replay limit.")
			}
			readings, err := historyReadings(ctx, tx, d, r.Body.RunId, r.Body.From, r.Body.To)
			if err != nil {
				return result, err
			}
			// Keep complete recorded intervals inside the request; do not fabricate boundary states.
			selected := readings[:0]
			for _, v := range readings {
				if !v.Sample.ObservedAt.Before(r.Body.From) && !v.Sample.ObservedAt.After(r.Body.To) {
					selected = append(selected, v)
				}
			}
			result, err = reporting.ReplayObserved(selected)
			if err != nil {
				return result, fail(422, "insufficient_evidence", err.Error())
			}
			result.ID, result.DeviceID, result.RunID, result.CreatedAt = uuid.NewString(), d.ID, r.Body.RunId, time.Now().UTC()
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO observed_replays(id,organization_id,device_id,report) VALUES($1,$2,$3,$4)`,
				result.ID,
				d.Org,
				d.ID,
				result,
			); err != nil {
				return result, err
			}
			return result, audit(ctx, tx, d.Org, s.userID, "observed_policy_replayed", result.ID, s.id)
		},
	)
	return api.CreateReplay201JSONResponse(report), err
}

func (a *App) ListReplays(ctx context.Context, r api.ListReplaysRequestObject) (api.ListReplaysResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT report FROM observed_replays WHERE device_id=$1 ORDER BY created_at DESC LIMIT 5`,
		r.DeviceId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []reporting.Replay{}
	for rows.Next() {
		var data []byte
		var report reporting.Replay
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &report); err != nil {
			return nil, err
		}
		result = append(result, report)
	}
	return api.ListReplays200JSONResponse(result), rows.Err()
}
