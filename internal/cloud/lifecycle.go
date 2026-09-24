package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"thermora.local/thermora/internal/identity"

	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

// fenceDevice preserves the sole command writer while invalidating unsent intentions.
// Callers hold the device row lock and the application's delivery fence.
func fenceDevice(ctx context.Context, tx pgx.Tx, d deviceRecord) error {
	if _, err := tx.Exec(
		ctx,
		`UPDATE commands SET stage='superseded',stage_rank=6 WHERE device_id=$1 AND stage_rank<4`,
		d.ID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		ctx,
		`DELETE FROM outbox WHERE device_id=$1 AND topic NOT LIKE '%/demo/action'`,
		d.ID,
	); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM optimization_plans WHERE device_id=$1`, d.ID)
	return err
}

func connected(ctx context.Context, q queryer, device string) (bool, error) {
	var allowed bool
	err := q.QueryRow(
		ctx,
		`SELECT NOT EXISTS(SELECT 1 FROM cloud_enrollments e JOIN manufacturer_connections c ON c.id=e.connection_id WHERE e.device_id=$1 AND (NOT e.control_connected OR c.status<>'connected'))`,
		device,
	).Scan(
		&allowed,
	)
	return allowed, err
}

func (a *App) UpdateBuildingProfile(
	ctx context.Context,
	r api.UpdateBuildingProfileRequestObject,
) (api.UpdateBuildingProfileResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	b, err := a.building(ctx, r.BuildingId, s.userID)
	if err != nil {
		return nil, err
	}
	p := r.Body.Profile
	for _, value := range []string{r.Body.Name, p.Address, p.City, p.PostalCode} {
		if strings.TrimSpace(value) == "" {
			return nil, fail(422, "invalid_building", "Complete the building details.")
		}
	}
	if r.Params.IfMatch != fmt.Sprintf(`"%d"`, b.Version) {
		return nil, fail(409, "stale_version", "Reload the building before saving.")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	tag, err := tx.Exec(
		ctx,
		`UPDATE buildings SET name=$2,version=version+1 WHERE id=$1 AND version=$3`,
		b.Id,
		strings.TrimSpace(r.Body.Name),
		b.Version,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, fail(409, "stale_version", "Reload the building before saving.")
	}
	oldTariff, err := tariff(ctx, tx, b.Id)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO building_profiles(building_id,organization_id,floor_area_m2,country_code,address,postal_code,city,tariff_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(building_id) DO UPDATE SET floor_area_m2=EXCLUDED.floor_area_m2,country_code=EXCLUDED.country_code,address=EXCLUDED.address,postal_code=EXCLUDED.postal_code,city=EXCLUDED.city,tariff_type=EXCLUDED.tariff_type`,
		b.Id,
		b.OrganizationId,
		p.FloorAreaM2,
		p.CountryCode,
		strings.TrimSpace(p.Address),
		strings.TrimSpace(p.PostalCode),
		strings.TrimSpace(p.City),
		p.TariffType,
	); err != nil {
		return nil, err
	}
	if oldTariff != string(p.TariffType) {
		for _, device := range b.Devices {
			d, loadErr := loadDevice(ctx, tx, device.Id, true)
			if loadErr != nil {
				return nil, loadErr
			}
			d.Config.Version++
			d.Config.Generation++
			d.Config.IssuedAt = time.Now().UTC()
			if err = fenceDevice(ctx, tx, d); err != nil {
				return nil, err
			}
			if _, err = tx.Exec(ctx, `UPDATE devices SET config=$2 WHERE id=$1`, d.ID, d.Config); err != nil {
				return nil, err
			}
			if err = queueConfig(ctx, tx, d); err != nil {
				return nil, err
			}
		}
	}
	if err = audit(ctx, tx, b.OrganizationId, s.userID, "building_profile_updated", b.Id, s.id); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	b, err = a.building(ctx, b.Id, s.userID)
	if err == nil {
		s.writer.Header().Set("ETag", fmt.Sprintf(`"%d"`, b.Version))
	}
	return api.UpdateBuildingProfile200JSONResponse(b), err
}

func (a *App) AttachManufacturerDevice(
	ctx context.Context,
	r api.AttachManufacturerDeviceRequestObject,
) (api.AttachManufacturerDeviceResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	b, err := a.building(ctx, r.BuildingId, s.userID)
	if err != nil {
		return nil, err
	}
	if !r.Body.ConsentAccepted || strings.TrimSpace(r.Body.ZoneName) == "" {
		return nil, fail(422, "consent_required", "Name the zone and accept local simulation terms.")
	}
	encoded, err := json.Marshal(r.Body)
	if err != nil {
		return nil, err
	}
	if cached, found, cacheErr := idempotencyResult[api.Device](
		ctx,
		a.DB,
		s.userID,
		"attach:"+b.Id,
		r.Params.IdempotencyKey,
		identity.Digest(string(encoded)),
	); found ||
		cacheErr != nil {
		return api.AttachManufacturerDevice201JSONResponse(cached), cacheErr
	}
	discovery, err := a.discovery(ctx, r.Body.ConnectionId)
	if err != nil {
		return nil, err
	}
	found := false
	for _, d := range discovery.Devices {
		if d.ID == r.Body.ManufacturerDeviceId {
			found = true
		}
	}
	if !found {
		return nil, missing()
	}
	d, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"attach:"+b.Id,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.Device, error) {
			var version int
			if err := tx.QueryRow(
				ctx,
				`SELECT version FROM buildings WHERE id=$1 FOR UPDATE`,
				b.Id,
			).Scan(
				&version,
			); err != nil {
				return api.Device{}, err
			}
			var count int
			if err := tx.QueryRow(
				ctx,
				`SELECT count(*) FROM devices WHERE building_id=$1`,
				b.Id,
			).Scan(
				&count,
			); err != nil {
				return api.Device{}, err
			}
			if count >= 25 {
				return api.Device{}, fail(409, "device_limit", "This local building is limited to 25 devices.")
			}
			var device, run string
			err := tx.QueryRow(
				ctx,
				`SELECT d.id,d.run_id FROM manufacturer_inventory i JOIN devices d ON d.id=i.device_id WHERE i.subject=$1 AND i.external_id=$2 AND d.organization_id=$3 AND d.enrollment='inventory' AND d.bootstrap_expires_at>now() FOR UPDATE OF d`,
				discovery.Subject,
				r.Body.ManufacturerDeviceId,
				b.OrganizationId,
			).Scan(
				&device,
				&run,
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return api.Device{}, fail(
					409,
					"device_unavailable",
					"Equipment is already claimed or belongs to another partner program.",
				)
			}
			if err != nil {
				return api.Device{}, err
			}
			cfg := protocol.NewConfiguration(
				device,
				run,
				protocol.Comfort{
					Target: float64(b.Preferences.ComfortTargetC),
					Min:    float64(b.Preferences.ComfortMinC),
					Max:    float64(b.Preferences.ComfortMaxC),
				},
			)
			if _, err = tx.Exec(
				ctx,
				`UPDATE devices SET name=$2,building_id=$3,enrollment='claimed',claim_hash=NULL,claim_expires_at=NULL,config=$4 WHERE id=$1`,
				device,
				strings.TrimSpace(r.Body.ZoneName),
				b.Id,
				cfg,
			); err != nil {
				return api.Device{}, err
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO cloud_enrollments(device_id,organization_id,connection_id,consent_version) VALUES($1,$2,$3,'local-demo-2026-09')`,
				device,
				b.OrganizationId,
				r.Body.ConnectionId,
			); err != nil {
				return api.Device{}, err
			}
			if err = audit(
				ctx,
				tx,
				b.OrganizationId,
				s.userID,
				"cloud_enrollment_consented",
				device,
				s.id,
			); err != nil {
				return api.Device{}, err
			}
			record, err := loadDevice(ctx, tx, device, false)
			return deviceView(record), err
		},
	)
	return api.AttachManufacturerDevice201JSONResponse(d), err
}

func withdrawDevice(ctx context.Context, tx pgx.Tx, id, user, requestID string) error {
	d, err := loadDevice(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if err = excludeFlexibility(ctx, tx, d.ID, "disconnected"); err != nil {
		return err
	}
	d.Config.Version++
	d.Config.Generation++
	d.Config.IssuedAt = time.Now().UTC()
	d.Config.Automation.Optimization = false
	if err = fenceDevice(ctx, tx, d); err != nil {
		return err
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE devices SET config=$2,override_target=NULL,override_until=NULL,last_received_at=NULL WHERE id=$1`,
		id,
		d.Config,
	); err != nil {
		return err
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE cloud_enrollments SET control_connected=false WHERE device_id=$1`,
		id,
	); err != nil {
		return err
	}
	return audit(ctx, tx, d.Org, user, "device_disconnected", id, requestID)
}

func (a *App) DisconnectDevice(
	ctx context.Context,
	r api.DisconnectDeviceRequestObject,
) (api.DisconnectDeviceResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	_, routeErr := a.manufacturerRoute(ctx, r.DeviceId)
	if routeErr != nil && !errors.Is(routeErr, pgx.ErrNoRows) {
		return nil, routeErr
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	if errors.Is(routeErr, pgx.ErrNoRows) {
		var pending bool
		if err = tx.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM run_operations WHERE device_id=$1 AND completed_at IS NULL)`,
			r.DeviceId,
		).Scan(
			&pending,
		); err != nil {
			return nil, err
		}
		if pending {
			return nil, fail(409, "run_pending", "Wait for the current run operation before retiring this identity.")
		}
	}
	if err = withdrawDevice(ctx, tx, r.DeviceId, s.userID, s.id); err != nil {
		return nil, err
	}
	if errors.Is(routeErr, pgx.ErrNoRows) {
		if _, err = tx.Exec(
			ctx,
			`UPDATE devices SET enrollment='revoked',claim_hash=NULL,bootstrap_hash=$2,credential_revocation_pending=(credential_hash IS NOT NULL),credential_hash=NULL WHERE id=$1`,
			r.DeviceId,
			identity.Digest("revoked"),
		); err != nil {
			return nil, err
		}
	}
	return api.DisconnectDevice204Response{}, tx.Commit(ctx)
}

func (a *App) ReconnectDevice(
	ctx context.Context,
	r api.ReconnectDeviceRequestObject,
) (api.ReconnectDeviceResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	if !r.Body.ConsentAccepted {
		return nil, fail(422, "consent_required", "Accept the local simulation terms to reconnect.")
	}
	route, err := a.manufacturerRoute(ctx, r.DeviceId)
	if err != nil {
		return nil, err
	}
	if route.User != s.userID || route.Connection != r.Body.ConnectionId {
		return nil, missing()
	}
	discovery, err := a.discovery(ctx, route.Connection)
	if err != nil {
		return nil, err
	}
	found := false
	for _, device := range discovery.Devices {
		if device.ID == r.DeviceId {
			found = true
		}
	}
	if !found {
		return nil, missing()
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
	d.Config.Version++
	d.Config.Generation++
	d.Config.IssuedAt = time.Now().UTC()
	if err = fenceDevice(ctx, tx, d); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE cloud_enrollments SET control_connected=true,consented_at=now() WHERE device_id=$1`,
		d.ID,
	); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE devices SET config=$2,last_received_at=NULL,accepted_version=0,reported_version=0 WHERE id=$1`,
		d.ID,
		d.Config,
	); err != nil {
		return nil, err
	}
	if err = queueConfig(ctx, tx, d); err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, d.Org, s.userID, "device_reconnected", d.ID, s.id); err != nil {
		return nil, err
	}
	return api.ReconnectDevice202Response{}, tx.Commit(ctx)
}

func (a *App) DisconnectManufacturer(
	ctx context.Context,
	r api.DisconnectManufacturerRequestObject,
) (api.DisconnectManufacturerResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	var user string
	if err = tx.QueryRow(
		ctx,
		`SELECT user_id FROM manufacturer_connections WHERE id=$1 AND user_id=$2`,
		r.ConnectionId,
		s.userID,
	).Scan(
		&user,
	); err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		ctx,
		`SELECT device_id FROM cloud_enrollments WHERE connection_id=$1 ORDER BY device_id`,
		r.ConnectionId,
	)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err = withdrawDevice(ctx, tx, id, s.userID, s.id); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE manufacturer_connections SET status='disconnected',revoked_at=now(),revocation_pending=(length(tokens)>0) WHERE id=$1`,
		r.ConnectionId,
	); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM manufacturer_authorizations WHERE user_id=$1`, s.userID); err != nil {
		return nil, err
	}
	return api.DisconnectManufacturer204Response{}, tx.Commit(ctx)
}

func (a *App) revokeDevices(ctx context.Context) error {
	rows, err := a.DB.Query(ctx, `SELECT id FROM devices WHERE credential_revocation_pending ORDER BY id LIMIT 25`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.Admin.DisableDevice(ctx, id); err != nil {
			return err
		}
		if _, err = a.DB.Exec(
			ctx,
			`UPDATE devices SET credential_revocation_pending=false WHERE id=$1 AND enrollment='revoked'`,
			id,
		); err != nil {
			return err
		}
	}
	return nil
}
