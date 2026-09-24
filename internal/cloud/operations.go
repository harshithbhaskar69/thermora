package cloud

import (
	"context"
	"encoding/json"
	"time"

	api "thermora.local/thermora/internal/transport/http/generated"
)

func (a *App) GetMetrics(ctx context.Context, r api.GetMetricsRequestObject) (api.GetMetricsResponseObject, error) {
	if err := a.operator(ctx, r.OrganizationId); err != nil {
		return nil, err
	}
	result := api.Metrics{SampledAtWall: time.Now().UTC()}
	err := a.DB.QueryRow(
		ctx,
		`SELECT count(*) FILTER(WHERE enrollment='active'),count(*) FILTER(WHERE enrollment='active' AND last_received_at>now()-interval '15 seconds'),COALESCE(sum(manufacturer_failures),0) FROM devices WHERE organization_id=$1`,
		r.OrganizationId,
	).Scan(
		&result.ActiveDevices,
		&result.FreshDevices,
		&result.ManufacturerFailures,
	)
	if err != nil {
		return nil, err
	}
	if err = a.DB.QueryRow(
		ctx,
		`SELECT count(*),COALESCE(EXTRACT(epoch FROM now()-min(created_at)),0)::float8 FROM outbox WHERE organization_id=$1`,
		r.OrganizationId,
	).Scan(
		&result.PendingDeliveries,
		&result.OldestDeliverySeconds,
	); err != nil {
		return nil, err
	}
	if err = a.DB.QueryRow(
		ctx,
		`SELECT count(*) FILTER(WHERE stage='rejected'),count(*) FILTER(WHERE stage='expired') FROM commands WHERE organization_id=$1 AND created_at>now()-interval '24 hours'`,
		r.OrganizationId,
	).Scan(
		&result.RejectedCommands,
		&result.ExpiredCommands,
	); err != nil {
		return nil, err
	}
	if err = a.DB.QueryRow(
		ctx,
		`SELECT count(*),avg(seconds)::float8 FROM (SELECT EXTRACT(epoch FROM min(e.received_at)-c.created_at) seconds FROM commands c JOIN command_events e ON e.command_id=c.id WHERE c.organization_id=$1 AND c.created_at>now()-interval '24 hours' AND e.payload->>'stage'='applied' GROUP BY c.id) timings`,
		r.OrganizationId,
	).Scan(
		&result.TurnaroundSamples,
		&result.MeanApplicationSeconds,
	); err != nil {
		return nil, err
	}
	var data []byte
	if err = a.DB.QueryRow(
		ctx,
		`SELECT COALESCE(jsonb_agg(jsonb_build_object('device_id',id,'telemetry_age_seconds',EXTRACT(epoch FROM now()-last_received_at),'credential_revocation_pending',credential_revocation_pending,'edge',diagnostics)),'[]') FROM (SELECT id,last_received_at,credential_revocation_pending,diagnostics FROM devices WHERE organization_id=$1 AND building_id IS NOT NULL ORDER BY id LIMIT 25) d`,
		r.OrganizationId,
	).Scan(
		&data,
	); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &result.DeviceDiagnostics); err != nil {
		return nil, err
	}
	return api.GetMetrics200JSONResponse(result), nil
}

// retain removes small committed batches. Replay reports remain immutable; old
// raw evidence is intentionally unavailable after this local retention boundary.
func (a *App) retain(ctx context.Context) error {
	for _, statement := range []string{
		`DELETE FROM telemetry WHERE ctid IN (SELECT t.ctid FROM telemetry t JOIN devices d ON d.id=t.device_id JOIN simulation_runs r ON r.id=t.run_id WHERE t.received_at<now()-interval '7 days' OR (r.finished_at IS NOT NULL AND r.epoch<d.run_epoch-20) LIMIT 5000)`,
		`DELETE FROM telemetry_batches WHERE ctid IN (SELECT ctid FROM telemetry_batches WHERE received_at<now()-interval '7 days' LIMIT 5000)`,
		`DELETE FROM command_events WHERE ctid IN (SELECT e.ctid FROM command_events e JOIN commands c ON c.id=e.command_id WHERE c.created_at<now()-interval '90 days' LIMIT 5000)`,
		`DELETE FROM commands WHERE ctid IN (SELECT c.ctid FROM commands c WHERE c.created_at<now()-interval '90 days' AND NOT EXISTS(SELECT 1 FROM command_events e WHERE e.command_id=c.id) AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.command_id=c.id) LIMIT 5000)`,
		`DELETE FROM audit WHERE ctid IN (SELECT ctid FROM audit WHERE recorded_at<now()-interval '90 days' LIMIT 5000)`,
		`DELETE FROM sessions WHERE ctid IN (SELECT ctid FROM sessions WHERE expires_at<now() LIMIT 5000)`,
		`DELETE FROM idempotency WHERE ctid IN (SELECT ctid FROM idempotency WHERE expires_at<now() LIMIT 5000)`,
		`DELETE FROM manufacturer_authorizations WHERE ctid IN (SELECT ctid FROM manufacturer_authorizations WHERE expires_at<now() LIMIT 5000)`,
	} {
		if _, err := a.DB.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
