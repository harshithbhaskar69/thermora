package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func (a *App) EnrollDevice(
	ctx context.Context,
	r api.EnrollDeviceRequestObject,
) (api.EnrollDeviceResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	b, err := a.building(ctx, r.BuildingId, s.userID)
	if err != nil {
		return nil, err
	}
	d, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"enroll:"+b.Id,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.Device, error) {
			var id, run string
			err := tx.QueryRow(
				ctx,
				`UPDATE devices SET building_id=$1,enrollment='claimed',claim_hash=NULL,claim_expires_at=NULL WHERE organization_id=$2 AND claim_hash=$3 AND claim_expires_at>now() AND enrollment='inventory' RETURNING id,run_id`,
				b.Id,
				b.OrganizationId,
				identity.Digest(r.Body.ClaimCode),
			).Scan(
				&id,
				&run,
			)
			if err != nil {
				return api.Device{}, fail(422, "invalid_claim", "The claim code is invalid, expired, or already used.")
			}
			cfg := protocol.NewConfiguration(
				id,
				run,
				protocol.Comfort{
					Target: float64(b.Preferences.ComfortTargetC),
					Min:    float64(b.Preferences.ComfortMinC),
					Max:    float64(b.Preferences.ComfortMaxC),
				},
			)
			if _, err = tx.Exec(ctx, `UPDATE devices SET config=$2 WHERE id=$1`, id, cfg); err != nil {
				return api.Device{}, err
			}
			if err = audit(ctx, tx, b.OrganizationId, s.userID, "device_claimed", id, s.id); err != nil {
				return api.Device{}, err
			}
			record, err := loadDevice(ctx, tx, id, false)
			return deviceView(record), err
		},
	)
	return api.EnrollDevice202JSONResponse(d), err
}

func (a *App) ActivateEdge(
	ctx context.Context,
	r api.ActivateEdgeRequestObject,
) (api.ActivateEdgeResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	auth := strings.TrimPrefix(request(ctx).request.Header.Get("Authorization"), "Bearer ")
	if len(auth) < 32 || len(auth) > 128 {
		return nil, fail(401, "invalid_bootstrap", "Invalid device bootstrap credential.")
	}
	digestData, err := json.Marshal(r.Body)
	if err != nil {
		return nil, err
	}
	digest := identity.Digest(string(digestData))
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, r.Body.DeviceId, true)
	if err != nil {
		return nil, fail(401, "invalid_bootstrap", "Invalid device bootstrap credential.")
	}
	var hash, attemptDigest []byte
	var attempt *string
	var expires time.Time
	var activated *time.Time
	if err = tx.QueryRow(
		ctx,
		`SELECT bootstrap_hash,bootstrap_expires_at,activation_id,activation_digest,activated_at FROM devices WHERE id=$1`,
		d.ID,
	).Scan(
		&hash,
		&expires,
		&attempt,
		&attemptDigest,
		&activated,
	); err != nil {
		return nil, err
	}
	if !bytes.Equal(hash, identity.Digest(auth)) || d.Enrollment == "revoked" {
		return nil, fail(401, "invalid_bootstrap", "Invalid device bootstrap credential.")
	}
	if d.Enrollment == "inventory" {
		return nil, fail(409, "ownership_pending", "Claim the equipment from a building first.")
	}
	if attempt != nil && (*attempt != r.Body.AttemptId || !bytes.Equal(attemptDigest, digest)) {
		return nil, fail(409, "activation_conflict", "This device already has an activation attempt.")
	}
	result := api.ActivateEdge200JSONResponse{
		DeviceId:       d.ID,
		OrganizationId: d.Org,
		RunId:          d.Run,
		RunEpoch:       int(d.Epoch),
	}
	if d.Enrollment == "active" {
		if activated == nil || time.Since(*activated) > 5*time.Minute {
			return nil, fail(401, "bootstrap_consumed", "The bootstrap credential has been consumed.")
		}
		return result, nil
	}
	if time.Now().After(expires) {
		return nil, fail(401, "bootstrap_expired", "The bootstrap credential has expired.")
	}
	if _, err = tx.Exec(
		ctx,
		`UPDATE devices SET enrollment='activating',activation_id=$2,activation_digest=$3,credential_hash=$4 WHERE id=$1`,
		d.ID,
		r.Body.AttemptId,
		digest,
		identity.Digest(r.Body.MqttPassword),
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if err = a.Admin.ProvisionDevice(ctx, d.Org, d.ID, r.Body.MqttPassword); err != nil {
		return nil, fail(503, "broker_unavailable", "Activation is pending; retry this same attempt.")
	}
	tx, err = a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(
		ctx,
		`UPDATE devices SET enrollment='active',activated_at=now() WHERE id=$1 AND enrollment='activating' AND activation_id=$2`,
		d.ID,
		r.Body.AttemptId,
	); err != nil {
		return nil, err
	}
	if err = queueConfig(ctx, tx, d); err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, d.Org, "", "device_activated", d.ID, request(ctx).id); err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}
