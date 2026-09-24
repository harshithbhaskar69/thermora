package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // Scratch images must validate IANA zones without a host zoneinfo database.

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

const buildingAccess = `EXISTS(SELECT 1 FROM memberships m WHERE m.organization_id=b.organization_id AND m.user_id=$2 AND (m.role='operator' OR EXISTS(SELECT 1 FROM building_grants g WHERE g.organization_id=b.organization_id AND g.building_id=b.id AND g.user_id=$2)))`

func (a *App) building(ctx context.Context, id, user string) (api.Building, error) {
	var b api.Building
	b.Devices = []api.Device{}
	err := a.DB.QueryRow(
		ctx,
		`SELECT b.id,b.organization_id,b.name,b.timezone,b.comfort_target,b.comfort_min,b.comfort_max,b.version FROM buildings b WHERE b.id=$1 AND `+buildingAccess,
		id,
		user,
	).Scan(
		&b.Id,
		&b.OrganizationId,
		&b.Name,
		&b.Preferences.Timezone,
		&b.Preferences.ComfortTargetC,
		&b.Preferences.ComfortMinC,
		&b.Preferences.ComfortMaxC,
		&b.Version,
	)
	if err != nil {
		return b, err
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT id FROM devices WHERE organization_id=$1 AND building_id=$2 ORDER BY name LIMIT 25`,
		b.OrganizationId,
		b.Id,
	)
	if err != nil {
		return b, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return b, err
	}
	for _, id := range ids {
		d, err := a.device(ctx, id, user)
		if err != nil {
			return b, err
		}
		b.Devices = append(b.Devices, d)
	}
	return b, nil
}

func (a *App) GetBuilding(ctx context.Context, r api.GetBuildingRequestObject) (api.GetBuildingResponseObject, error) {
	b, err := a.building(ctx, r.BuildingId, request(ctx).userID)
	if err == nil {
		request(ctx).writer.Header().Set("ETag", fmt.Sprintf(`"%d"`, b.Version))
	}
	return api.GetBuilding200JSONResponse(b), err
}

func (a *App) ListBuildings(
	ctx context.Context,
	r api.ListBuildingsRequestObject,
) (api.ListBuildingsResponseObject, error) {
	user := request(ctx).userID
	var member bool
	if err := a.DB.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM memberships WHERE organization_id=$1 AND user_id=$2)`,
		r.OrganizationId,
		user,
	).Scan(
		&member,
	); err != nil {
		return nil, err
	}
	if !member {
		return nil, missing()
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT b.id FROM buildings b WHERE b.organization_id=$1 AND `+buildingAccess+` ORDER BY b.name LIMIT 200`,
		r.OrganizationId,
		user,
	)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	buildings := []api.Building{}
	for _, id := range ids {
		b, err := a.building(ctx, id, user)
		if err != nil {
			return nil, err
		}
		buildings = append(buildings, b)
	}
	return api.ListBuildings200JSONResponse(buildings), nil
}

func (a *App) CreateBuilding(
	ctx context.Context,
	r api.CreateBuildingRequestObject,
) (api.CreateBuildingResponseObject, error) {
	if _, err := time.LoadLocation(r.Body.Timezone); err != nil {
		return nil, fail(422, "invalid_timezone", "Choose an IANA timezone.")
	}
	name := strings.TrimSpace(r.Body.Name)
	if name == "" {
		return nil, fail(422, "invalid_name", "Enter a building name.")
	}
	s := request(ctx)
	var role string
	if err := a.DB.QueryRow(
		ctx,
		`SELECT role FROM memberships WHERE organization_id=$1 AND user_id=$2`,
		r.OrganizationId,
		s.userID,
	).Scan(
		&role,
	); err != nil {
		return nil, err
	}
	b, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"create-building:"+r.OrganizationId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.Building, error) {
			b := api.Building{
				Id:             uuid.NewString(),
				OrganizationId: r.OrganizationId,
				Name:           name,
				Version:        1,
				Preferences: api.Preferences{
					Timezone:       r.Body.Timezone,
					ComfortTargetC: 21,
					ComfortMinC:    20,
					ComfortMaxC:    22,
				},
				Devices: []api.Device{},
			}
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO buildings(id,organization_id,name,timezone) VALUES($1,$2,$3,$4)`,
				b.Id,
				b.OrganizationId,
				b.Name,
				b.Preferences.Timezone,
			); err != nil {
				return b, err
			}
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO building_grants(organization_id,building_id,user_id) VALUES($1,$2,$3)`,
				b.OrganizationId,
				b.Id,
				s.userID,
			); err != nil {
				return b, err
			}
			return b, audit(ctx, tx, b.OrganizationId, s.userID, "building_created", b.Id, s.id)
		},
	)
	return api.CreateBuilding201JSONResponse(b), err
}

func (a *App) UpdatePreferences(
	ctx context.Context,
	r api.UpdatePreferencesRequestObject,
) (api.UpdatePreferencesResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	b, err := a.building(ctx, r.BuildingId, s.userID)
	if err != nil {
		return nil, err
	}
	c := protocol.Comfort{
		Target: float64(r.Body.ComfortTargetC),
		Min:    float64(r.Body.ComfortMinC),
		Max:    float64(r.Body.ComfortMaxC),
	}
	if err = c.Validate(); err != nil {
		return nil, fail(422, "invalid_comfort", err.Error())
	}
	if _, err = time.LoadLocation(r.Body.Timezone); err != nil {
		return nil, fail(422, "invalid_timezone", "Choose an IANA timezone.")
	}
	if r.Params.IfMatch != fmt.Sprintf(`"%d"`, b.Version) {
		return nil, fail(409, "stale_version", "Reload the preferences before saving.")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	tag, err := tx.Exec(
		ctx,
		`UPDATE buildings SET timezone=$2,comfort_target=$3,comfort_min=$4,comfort_max=$5,version=version+1 WHERE id=$1 AND version=$6`,
		b.Id,
		r.Body.Timezone,
		c.Target,
		c.Min,
		c.Max,
		b.Version,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, fail(409, "stale_version", "Reload the preferences before saving.")
	}
	for _, device := range b.Devices {
		d, err := loadDevice(ctx, tx, device.Id, true)
		if err != nil {
			return nil, err
		}
		if err = excludeFlexibility(ctx, tx, d.ID, "preferences_changed"); err != nil {
			return nil, err
		}
		d.Config.Version++
		d.Config.Generation++
		d.Config.Comfort = c
		d.Config.IssuedAt = time.Now().UTC()
		if _, err = tx.Exec(
			ctx,
			`UPDATE devices SET config=$2,override_target=NULL,override_until=NULL WHERE id=$1`,
			d.ID,
			d.Config,
		); err != nil {
			return nil, err
		}
		if err = fenceDevice(ctx, tx, d); err != nil {
			return nil, err
		}
		if err = queueConfig(ctx, tx, d); err != nil {
			return nil, err
		}
	}
	if err = audit(ctx, tx, b.OrganizationId, s.userID, "preferences_changed", b.Id, s.id); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	b, err = a.building(ctx, b.Id, s.userID)
	if err == nil {
		request(ctx).writer.Header().Set("ETag", fmt.Sprintf(`"%d"`, b.Version))
	}
	return api.UpdatePreferences200JSONResponse(b), err
}

func audit(ctx context.Context, tx pgx.Tx, org, user, action, resource, id string) error {
	_, err := tx.Exec(
		ctx,
		`INSERT INTO audit(organization_id,actor_id,action,resource_id,request_id) VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5)`,
		org,
		user,
		action,
		resource,
		id,
	)
	return err
}

func idempotent[T any](
	ctx context.Context,
	db *pgxpool.Pool,
	actor, operation, key string,
	body any,
	fn func(pgx.Tx) (T, error),
) (T, error) {
	var result T
	data, err := json.Marshal(body)
	if err != nil {
		return result, err
	}
	digest := identity.Digest(string(data))
	tx, err := db.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		actor+":"+operation+":"+key,
	); err != nil {
		return result, err
	}
	if cached, found, cacheErr := idempotencyResult[T](
		ctx,
		tx,
		actor,
		operation,
		key,
		digest,
	); found ||
		cacheErr != nil {
		return cached, cacheErr
	}
	result, err = fn(tx)
	if err != nil {
		return result, err
	}
	response, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO idempotency(actor_id,operation,key,digest,response) VALUES($1,$2,$3,$4,$5) ON CONFLICT(actor_id,operation,key) DO UPDATE SET digest=excluded.digest,response=excluded.response,expires_at=excluded.expires_at`,
		actor,
		operation,
		key,
		digest,
		response,
	); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

// idempotencyResult also permits a cached reply before a fallible external call.
// The mutation still rechecks under idempotent's transaction lock before committing.
func idempotencyResult[T any](
	ctx context.Context,
	q queryer,
	actor, operation, key string,
	digest []byte,
) (T, bool, error) {
	var result T
	var previous, response []byte
	err := q.QueryRow(
		ctx,
		`SELECT digest,response FROM idempotency WHERE actor_id=$1 AND operation=$2 AND key=$3 AND expires_at>now()`,
		actor,
		operation,
		key,
	).Scan(
		&previous,
		&response,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if !bytes.Equal(previous, digest) {
		return result, true, fail(
			409,
			"idempotency_conflict",
			"This request key has already been used with different input.",
		)
	}
	err = json.Unmarshal(response, &result)
	return result, true, err
}
