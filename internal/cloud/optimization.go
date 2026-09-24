package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/optimization"
	"thermora.local/thermora/internal/scenario"
	"thermora.local/thermora/internal/sim"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func tariff(ctx context.Context, q queryer, building string) (string, error) {
	var value string
	err := q.QueryRow(ctx, `SELECT tariff_type FROM building_profiles WHERE building_id=$1`, building).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "variable", nil
	}
	return value, err
}

func (a *App) optimizationState(ctx context.Context, d deviceRecord) (api.OptimizationState, error) {
	t, err := tariff(ctx, a.DB, d.Building)
	if err != nil {
		return api.OptimizationState{}, err
	}
	r := api.OptimizationState{
		Enabled:            d.Config.Automation.Optimization,
		Scenario:           scenario.Names[d.Config.WeatherProfile],
		Tariff:             api.OptimizationStateTariff(t),
		IntegrationRoute:   "edge",
		ManufacturerStatus: "not_applicable",
		ArtifactsEnabled:   a.Artifacts != nil,
	}
	route, err := a.manufacturerRoute(ctx, d.ID)
	if err == nil {
		r.IntegrationRoute = "manufacturer"
		if err = a.DB.QueryRow(
			ctx,
			`SELECT status FROM manufacturer_connections WHERE id=$1`,
			route.Connection,
		).Scan(
			&r.ManufacturerStatus,
		); err != nil {
			return r, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	var data []byte
	err = a.DB.QueryRow(
		ctx,
		`SELECT plan FROM optimization_plans WHERE organization_id=$1 AND device_id=$2 AND run_id=$3 AND config_version=$4`,
		d.Org,
		d.ID,
		d.Run,
		d.Config.Version,
	).Scan(
		&data,
	)
	if err == nil {
		var p optimization.Plan
		err = json.Unmarshal(data, &p)
		r.Plan = &p
	} else if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return r, err
}

func (a *App) GetOptimization(
	ctx context.Context,
	r api.GetOptimizationRequestObject,
) (api.GetOptimizationResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	d, err := loadDevice(ctx, a.DB, r.DeviceId, false)
	if err != nil {
		return nil, err
	}
	state, err := a.optimizationState(ctx, d)
	return api.GetOptimization200JSONResponse(state), err
}

func (a *App) SetOptimization(
	ctx context.Context,
	r api.SetOptimizationRequestObject,
) (api.SetOptimizationResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	code, ok := scenario.Code(string(r.Body.Scenario))
	if !ok || code == 0 {
		return nil, fail(422, "invalid_scenario", "Choose a packaged scenario.")
	}
	_, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"optimization:"+r.DeviceId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (bool, error) {
			d, err := loadDevice(ctx, tx, r.DeviceId, true)
			if err != nil {
				return false, err
			}
			allowed, err := connected(ctx, tx, d.ID)
			if err != nil {
				return false, err
			}
			if r.Body.Enabled && !allowed {
				return false, fail(409, "device_disconnected", "Reconnect the device before enabling automation.")
			}
			if r.Body.Enabled && (d.Latest == nil || d.Received == nil || time.Since(*d.Received) > 15*time.Second) {
				return false, fail(409, "device_not_ready", "Wait for fresh equipment telemetry.")
			}
			if !r.Body.Enabled {
				if err = excludeFlexibility(ctx, tx, d.ID, "automation_disabled"); err != nil {
					return false, err
				}
			}
			d.Config.Version++
			d.Config.Generation++
			d.Config.IssuedAt = time.Now().UTC()
			d.Config.WeatherProfile = code
			d.Config.Automation.Optimization = r.Body.Enabled
			if _, err = tx.Exec(ctx, `UPDATE devices SET config=$2 WHERE id=$1`, d.ID, d.Config); err != nil {
				return false, err
			}
			if err = queueConfig(ctx, tx, d); err != nil {
				return false, err
			}
			return true, audit(ctx, tx, d.Org, s.userID, "optimization_updated", d.ID, s.id)
		},
	)
	if err != nil {
		return nil, err
	}
	d, err := loadDevice(ctx, a.DB, r.DeviceId, false)
	if err != nil {
		return nil, err
	}
	state, err := a.optimizationState(ctx, d)
	return api.SetOptimization202JSONResponse(state), err
}

// activePlan runs under the existing device lock, so a preference edit cannot race the plan inputs.
func activePlan(ctx context.Context, tx pgx.Tx, d deviceRecord) (optimization.Plan, error) {
	var p optimization.Plan
	var data []byte
	err := tx.QueryRow(
		ctx,
		`SELECT plan FROM optimization_plans WHERE device_id=$1 AND organization_id=$2 AND run_id=$3 AND config_version=$4 AND valid_from<=$5 AND valid_until>$5`,
		d.ID,
		d.Org,
		d.Run,
		d.Config.Version,
		d.Latest.ObservedAt,
	).Scan(
		&data,
	)
	if err == nil {
		err = json.Unmarshal(data, &p)
		return p, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	t, err := tariff(ctx, tx, d.Building)
	if err != nil {
		return p, err
	}
	initial := sim.NewState(d.Epoch)
	initial.Indoor, initial.Supply = d.Latest.Indoor, d.Latest.Supply
	if d.Config.HotWater.Enabled {
		initial.Model = "heating-dhw"
		initial.DHWStart, initial.DHWStop = d.Config.HotWater.Start, d.Config.HotWater.Stop
		if d.Latest.HotWater == nil || d.Latest.Buffer == nil {
			return p, errors.New("waiting for tank telemetry")
		}
		initial.Tank, initial.Buffer, initial.DHW = d.Latest.HotWater.Temperature, *d.Latest.Buffer, d.Latest.HotWater.Heating
	}
	p, err = optimization.BuildForState(
		scenario.Names[d.Config.WeatherProfile],
		t,
		d.Latest.ObservedAt,
		d.Config.Comfort,
		initial,
		d.Config.Preset,
	)
	if err != nil {
		return p, err
	}
	_, err = tx.Exec(
		ctx,
		`INSERT INTO optimization_plans(device_id,organization_id,run_id,config_version,valid_from,valid_until,plan) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(device_id) DO UPDATE SET run_id=EXCLUDED.run_id,config_version=EXCLUDED.config_version,valid_from=EXCLUDED.valid_from,valid_until=EXCLUDED.valid_until,plan=EXCLUDED.plan`,
		d.ID,
		d.Org,
		d.Run,
		d.Config.Version,
		p.Start,
		p.Start.Add(15*time.Minute),
		p,
	)
	return p, err
}

func (a *App) CreateComparison(
	ctx context.Context,
	r api.CreateComparisonRequestObject,
) (api.CreateComparisonResponseObject, error) {
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	report, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"comparison:"+r.DeviceId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.Comparison, error) {
			d, err := loadDevice(ctx, tx, r.DeviceId, true)
			if err != nil {
				return api.Comparison{}, err
			}
			var count int
			if err = tx.QueryRow(
				ctx,
				`SELECT count(*) FROM comparisons WHERE organization_id=$1 AND device_id=$2`,
				d.Org,
				d.ID,
			).Scan(
				&count,
			); err != nil {
				return api.Comparison{}, err
			}
			if count >= 100 {
				return api.Comparison{}, fail(
					409,
					"comparison_limit",
					"This local device has reached its 100-report limit.",
				)
			}
			t, err := tariff(ctx, tx, d.Building)
			if err != nil {
				return api.Comparison{}, err
			}
			initial := sim.NewState(1)
			if d.Config.HotWater.Enabled {
				initial.Model = "heating-dhw"
				initial.DHWStart, initial.DHWStop = d.Config.HotWater.Start, d.Config.HotWater.Stop
			}
			p, err := optimization.BuildForState(
				string(r.Body.Scenario),
				t,
				r.Body.Start,
				d.Config.Comfort,
				initial,
				d.Config.Preset,
			)
			if err != nil {
				return api.Comparison{}, fail(422, "invalid_comparison", err.Error())
			}
			report := api.Comparison{Id: uuid.NewString(), DeviceId: d.ID, CreatedAtWall: time.Now().UTC(), Plan: p}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO comparisons(id,organization_id,device_id,report) VALUES($1,$2,$3,$4)`,
				report.Id,
				d.Org,
				d.ID,
				report,
			); err != nil {
				return report, err
			}
			return report, audit(ctx, tx, d.Org, s.userID, "comparison_created", report.Id, s.id)
		},
	)
	return api.CreateComparison201JSONResponse(report), err
}

func (a *App) ListComparisons(
	ctx context.Context,
	r api.ListComparisonsRequestObject,
) (api.ListComparisonsResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT report FROM comparisons WHERE device_id=$1 ORDER BY created_at DESC LIMIT 5`,
		r.DeviceId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := []api.Comparison{}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var report api.Comparison
		if err = json.Unmarshal(data, &report); err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return api.ListComparisons200JSONResponse(reports), rows.Err()
}
