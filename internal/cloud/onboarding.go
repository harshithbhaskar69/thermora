package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func (a *App) ListManufacturers(
	_ context.Context,
	_ api.ListManufacturersRequestObject,
) (api.ListManufacturersResponseObject, error) {
	result := api.ListManufacturers200JSONResponse{}
	if a.Manufacturer != nil {
		result = append(result, struct {
			Id            string `json:"id"`
			Name          string `json:"name"`
			Prerequisites string `json:"prerequisites"`
			Simulated     bool   `json:"simulated"`
		}{
			Id:            "demo",
			Name:          "Thermora demo manufacturer",
			Prerequisites: "Use the separately provisioned local manufacturer account. Its simulated heat pump must be online. No real manufacturer is contacted.",
			Simulated:     true,
		})
	}
	return result, nil
}

func (a *App) AuthorizeManufacturer(
	ctx context.Context,
	_ api.AuthorizeManufacturerRequestObject,
) (api.AuthorizeManufacturerResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if a.Manufacturer == nil {
		return nil, fail(503, "integration_unavailable", "The local manufacturer integration is not configured.")
	}
	s := request(ctx)
	state, browser, verifier := local.Secret(), local.Secret(), local.Secret()
	expires := time.Now().UTC().Add(5 * time.Minute)
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	// One pending flow per user; restart replaces abandoned authorization, never a connected device.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "oauth:"+s.userID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`DELETE FROM manufacturer_authorizations WHERE user_id=$1 OR expires_at<now()`,
		s.userID,
	); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO manufacturer_authorizations(state_hash,user_id,browser_hash,verifier,expires_at) VALUES($1,$2,$3,$4,$5)`,
		identity.Digest(state),
		s.userID,
		identity.Digest(browser),
		a.Manufacturer.Encrypt([]byte(verifier), "oauth:"+s.userID),
		expires,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	http.SetCookie(
		s.writer,
		&http.Cookie{
			Name:     "thermora_oauth",
			Value:    browser,
			Path:     "/api/v1/manufacturer-authorizations/callback",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   300,
		},
	)
	return api.AuthorizeManufacturer201JSONResponse{
		AuthorizationUrl: a.Manufacturer.AuthorizationURL(state, verifier),
		ExpiresAtWall:    expires,
	}, nil
}

func callbackRedirect(location string) api.ManufacturerCallback303Response {
	return api.ManufacturerCallback303Response{Headers: api.ManufacturerCallback303ResponseHeaders{Location: location}}
}

func (a *App) ManufacturerCallback(
	ctx context.Context,
	r api.ManufacturerCallbackRequestObject,
) (api.ManufacturerCallbackResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if a.Manufacturer == nil {
		return nil, missing()
	}
	s := request(ctx)
	cookie, err := s.request.Cookie("thermora_oauth")
	if err != nil {
		return nil, fail(400, "invalid_oauth_state", "Restart the connection from this browser.")
	}
	var user string
	var encrypted []byte
	var authorizedAt time.Time
	err = a.DB.QueryRow(
		ctx,
		`DELETE FROM manufacturer_authorizations WHERE state_hash=$1 AND browser_hash=$2 AND expires_at>now() RETURNING user_id,verifier,created_at`,
		identity.Digest(r.Params.State),
		identity.Digest(cookie.Value),
	).Scan(
		&user,
		&encrypted,
		&authorizedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(
			400,
			"invalid_oauth_state",
			"Authorization expired or was already used. Restart the connection.",
		)
	}
	if err != nil {
		return nil, err
	}
	http.SetCookie(
		s.writer,
		&http.Cookie{
			Name:     "thermora_oauth",
			Path:     "/api/v1/manufacturer-authorizations/callback",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		},
	)
	if r.Params.Error != nil || r.Params.Code == nil {
		return callbackRedirect("/connect?error=authorization_denied"), nil
	}
	verifier, err := a.Manufacturer.Decrypt(encrypted, "oauth:"+user)
	if err != nil {
		return nil, err
	}
	tokens, err := a.Manufacturer.Exchange(ctx, *r.Params.Code, string(verifier))
	if err != nil {
		return callbackRedirect("/connect?error=authorization_failed"), nil
	}
	discovery, err := a.Manufacturer.Discover(ctx, tokens.AccessToken)
	if err != nil {
		return callbackRedirect("/connect?error=discovery_failed"), nil
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "connection:"+user); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	var existing string
	err = tx.QueryRow(
		ctx,
		`SELECT id FROM manufacturer_connections WHERE user_id=$1 AND provider='demo' AND subject=$2`,
		user,
		discovery.Subject,
	).Scan(
		&existing,
	)
	if err == nil {
		var revoked *time.Time
		var pending bool
		if err = tx.QueryRow(
			ctx,
			`SELECT revoked_at,revocation_pending FROM manufacturer_connections WHERE id=$1`,
			existing,
		).Scan(
			&revoked,
			&pending,
		); err != nil {
			return nil, err
		}
		if pending || (revoked != nil && !authorizedAt.After(*revoked)) {
			return callbackRedirect("/connect?error=authorization_revoked"), nil
		}
		id = existing
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// #nosec G117 -- credentials are immediately AEAD-encrypted with a connection binding, never logged or returned to browsers.
	data, err := json.Marshal(tokens)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO manufacturer_connections(id,user_id,provider,subject,tokens,expires_at) VALUES($1,$2,'demo',$3,$4,$5) ON CONFLICT(id) DO UPDATE SET tokens=EXCLUDED.tokens,expires_at=EXCLUDED.expires_at,status='connected'`,
		id,
		user,
		discovery.Subject,
		a.Manufacturer.Encrypt(data, "tokens:"+id),
		time.Now().Add(time.Duration(tokens.ExpiresIn)*time.Second),
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return callbackRedirect("/connect?connection=" + id), nil
}

func (a *App) ListManufacturerConnections(
	ctx context.Context,
	_ api.ListManufacturerConnectionsRequestObject,
) (api.ListManufacturerConnectionsResponseObject, error) {
	rows, err := a.DB.Query(
		ctx,
		`SELECT id,provider,status,created_at FROM manufacturer_connections WHERE user_id=$1 ORDER BY created_at LIMIT 25`,
		request(ctx).userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := api.ListManufacturerConnections200JSONResponse{}
	for rows.Next() {
		var c api.ManufacturerConnection
		if err = rows.Scan(&c.Id, &c.ManufacturerId, &c.Status, &c.CreatedAtWall); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// connectionCredentials serializes token rotation and commits before equipment work.
func (a *App) connectionCredentials(ctx context.Context, id, user string) (manufacturerCredentials, error) {
	var result manufacturerCredentials
	if a.Manufacturer == nil {
		return result, fail(503, "integration_unavailable", "The manufacturer integration is not configured.")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer storage.Rollback(ctx, tx)
	var encrypted []byte
	var expires time.Time
	var status, subject string
	err = tx.QueryRow(
		ctx,
		`SELECT tokens,expires_at,status,subject FROM manufacturer_connections WHERE id=$1 AND user_id=$2 FOR UPDATE`,
		id,
		user,
	).Scan(
		&encrypted,
		&expires,
		&status,
		&subject,
	)
	if err != nil {
		return result, err
	}
	if status != "connected" {
		return result, fail(409, "manufacturer_authentication_required", "Reconnect your manufacturer account.")
	}
	data, err := a.Manufacturer.Decrypt(encrypted, "tokens:"+id)
	if err != nil {
		return result, err
	}
	var tokens manufacturer.Tokens
	if err = json.Unmarshal(data, &tokens); err != nil {
		return result, err
	}
	if time.Until(expires) < 30*time.Second {
		tokens, err = a.Manufacturer.Refresh(ctx, tokens.RefreshToken)
		if errors.Is(err, manufacturer.ErrAuthorization) {
			if _, err = tx.Exec(
				ctx,
				`UPDATE manufacturer_connections SET status='authentication_required',tokens=''::bytea WHERE id=$1`,
				id,
			); err != nil {
				return result, err
			}
			if err = tx.Commit(ctx); err != nil {
				return result, err
			}
			return result, fail(409, "manufacturer_authentication_required", "Reconnect your manufacturer account.")
		}
		if err != nil {
			return result, fail(503, "manufacturer_unavailable", "The manufacturer is unavailable. Try again shortly.")
		}
		// #nosec G117 -- the rotated credentials are immediately AEAD-encrypted before persistence.
		data, err = json.Marshal(tokens)
		if err != nil {
			return result, err
		}
		encrypted = a.Manufacturer.Encrypt(data, "tokens:"+id)
		if _, err = tx.Exec(
			ctx,
			`UPDATE manufacturer_connections SET tokens=$2,expires_at=$3 WHERE id=$1`,
			id,
			encrypted,
			time.Now().Add(time.Duration(tokens.ExpiresIn)*time.Second),
		); err != nil {
			return result, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return manufacturerCredentials{
		id:        id,
		user:      user,
		subject:   subject,
		encrypted: encrypted,
		token:     tokens.AccessToken,
	}, nil
}

type manufacturerCredentials struct {
	id, user, subject, token string
	encrypted                []byte
}

func (a *App) manufacturerError(ctx context.Context, auth manufacturerCredentials, err error) error {
	if errors.Is(err, manufacturer.ErrAuthorization) {
		if _, e := a.DB.Exec(
			ctx,
			`UPDATE manufacturer_connections SET status='authentication_required',tokens=''::bytea WHERE id=$1 AND user_id=$2 AND tokens=$3 AND status='connected'`,
			auth.id,
			auth.user,
			auth.encrypted,
		); e != nil {
			return e
		}
		return fail(
			409,
			"manufacturer_authentication_required",
			"Reconnect your manufacturer account to authorize equipment control.",
		)
	}
	if err != nil {
		return fail(503, "manufacturer_unavailable", "The manufacturer is unavailable. Try again shortly.")
	}
	return nil
}

func (a *App) discovery(ctx context.Context, id string) (manufacturer.Discovery, error) {
	auth, err := a.connectionCredentials(ctx, id, request(ctx).userID)
	if err != nil {
		return manufacturer.Discovery{}, err
	}
	result, err := a.Manufacturer.Discover(ctx, auth.token)
	if err = a.manufacturerError(ctx, auth, err); err != nil {
		return result, err
	}
	if result.Subject != auth.subject {
		return result, fail(503, "manufacturer_unavailable", "The manufacturer could not provide device information.")
	}
	return result, nil
}

func (a *App) DiscoverManufacturerDevices(
	ctx context.Context,
	r api.DiscoverManufacturerDevicesRequestObject,
) (api.DiscoverManufacturerDevicesResponseObject, error) {
	discovery, err := a.discovery(ctx, r.ConnectionId)
	if err != nil {
		return nil, err
	}
	result := api.DiscoverManufacturerDevices200JSONResponse{}
	for _, d := range discovery.Devices {
		var available bool
		err = a.DB.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM manufacturer_inventory i JOIN devices d ON d.id=i.device_id WHERE i.provider='demo' AND i.subject=$1 AND i.external_id=$2 AND d.enrollment='inventory' AND d.bootstrap_expires_at>now())`,
			discovery.Subject,
			d.ID,
		).Scan(
			&available,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, struct {
			Available bool   `json:"available"`
			Id        string `json:"id"`
			Name      string `json:"name"`
		}{available, d.ID, d.Name})
	}
	return result, nil
}

func (a *App) CompleteCloudEnrollment(
	ctx context.Context,
	r api.CompleteCloudEnrollmentRequestObject,
) (api.CompleteCloudEnrollmentResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s, body := request(ctx), r.Body
	if !body.ConsentAccepted {
		return nil, fail(422, "consent_required", "Review and accept the local simulation terms before connecting.")
	}
	if body.AccessCode == nil {
		return nil, fail(422, "invalid_access_code", "Enter the partner access code.")
	}
	if _, err := time.LoadLocation(body.Timezone); err != nil {
		return nil, fail(422, "invalid_timezone", "Choose a valid IANA timezone.")
	}
	for _, value := range []string{body.BuildingName, body.Profile.Address, body.Profile.City, body.Profile.PostalCode} {
		if strings.TrimSpace(value) == "" {
			return nil, fail(422, "invalid_building", "Complete the building details.")
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if cached, found, cacheErr := idempotencyResult[api.Building](
		ctx,
		a.DB,
		s.userID,
		"cloud-enrollment",
		r.Params.IdempotencyKey,
		identity.Digest(string(encoded)),
	); found ||
		cacheErr != nil {
		return api.CompleteCloudEnrollment201JSONResponse(cached), cacheErr
	}
	// Finish refresh/discovery without holding an enrollment transaction or a second pool connection.
	// Token rotation must remain committed even when a later entitlement check fails.
	discovery, err := a.discovery(ctx, body.ConnectionId)
	if err != nil {
		return nil, err
	}
	view, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"cloud-enrollment",
		r.Params.IdempotencyKey,
		body,
		func(tx pgx.Tx) (api.Building, error) {
			found := false
			for _, d := range discovery.Devices {
				if d.ID == body.ManufacturerDeviceId {
					found = true
				}
			}
			if !found {
				return api.Building{}, missing()
			}
			var org string
			err = tx.QueryRow(
				ctx,
				`UPDATE partner_invitations SET redeemed_by=$2,redeemed_at=now() WHERE code_hash=$1 AND expires_at>now() AND redeemed_by IS NULL RETURNING organization_id`,
				identity.Digest(*body.AccessCode),
				s.userID,
			).Scan(
				&org,
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return api.Building{}, fail(
					422,
					"invalid_access_code",
					"The partner access code is invalid, expired, or already used. It is separate from an equipment claim code.",
				)
			}
			if err != nil {
				return api.Building{}, err
			}
			var device, run string
			err = tx.QueryRow(
				ctx,
				`SELECT d.id,d.run_id FROM manufacturer_inventory i JOIN devices d ON d.id=i.device_id WHERE i.provider='demo' AND i.subject=$1 AND i.external_id=$2 AND d.organization_id=$3 AND d.enrollment='inventory' AND d.bootstrap_expires_at>now() FOR UPDATE OF d`,
				discovery.Subject,
				body.ManufacturerDeviceId,
				org,
			).Scan(
				&device,
				&run,
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return api.Building{}, fail(
					409,
					"device_unavailable",
					"This device is already connected, unavailable, or belongs to another partner program.",
				)
			}
			if err != nil {
				return api.Building{}, err
			}
			id := uuid.NewString()
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO memberships(organization_id,user_id,role) VALUES($1,$2,'owner') ON CONFLICT DO NOTHING`,
				org,
				s.userID,
			); err != nil {
				return api.Building{}, err
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO buildings(id,organization_id,name,timezone) VALUES($1,$2,$3,$4)`,
				id,
				org,
				strings.TrimSpace(body.BuildingName),
				body.Timezone,
			); err != nil {
				return api.Building{}, err
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO building_grants(organization_id,building_id,user_id) VALUES($1,$2,$3)`,
				org,
				id,
				s.userID,
			); err != nil {
				return api.Building{}, err
			}
			p := body.Profile
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO building_profiles(building_id,organization_id,floor_area_m2,country_code,address,postal_code,city,tariff_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
				id,
				org,
				p.FloorAreaM2,
				p.CountryCode,
				strings.TrimSpace(p.Address),
				strings.TrimSpace(p.PostalCode),
				strings.TrimSpace(p.City),
				p.TariffType,
			); err != nil {
				return api.Building{}, err
			}
			cfg := protocol.NewConfiguration(device, run, protocol.Comfort{Target: 21, Min: 20, Max: 22})
			if _, err = tx.Exec(
				ctx,
				`UPDATE devices SET enrollment='claimed',building_id=$2,claim_hash=NULL,claim_expires_at=NULL,config=$3 WHERE id=$1`,
				device,
				id,
				cfg,
			); err != nil {
				return api.Building{}, err
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO cloud_enrollments(device_id,organization_id,connection_id,consent_version) VALUES($1,$2,$3,$4)`,
				device,
				org,
				body.ConnectionId,
				body.ConsentVersion,
			); err != nil {
				return api.Building{}, err
			}
			if err = audit(ctx, tx, org, s.userID, "cloud_enrollment_consented", device, s.id); err != nil {
				return api.Building{}, err
			}
			return api.Building{
				Id:             id,
				OrganizationId: org,
				Name:           strings.TrimSpace(body.BuildingName),
				Version:        1,
				Preferences: api.Preferences{
					Timezone:       body.Timezone,
					ComfortTargetC: 21,
					ComfortMinC:    20,
					ComfortMaxC:    22,
				},
				Devices: []api.Device{
					deviceView(
						deviceRecord{
							ID:         device,
							Org:        org,
							Building:   id,
							Name:       "Heat pump 01",
							Enrollment: "claimed",
							Run:        run,
							Config:     cfg,
						},
					),
				},
			}, nil
		},
	)
	return api.CompleteCloudEnrollment201JSONResponse(view), err
}

func (a *App) GetBuildingProfile(
	ctx context.Context,
	r api.GetBuildingProfileRequestObject,
) (api.GetBuildingProfileResponseObject, error) {
	if _, err := a.building(ctx, r.BuildingId, request(ctx).userID); err != nil {
		return nil, err
	}
	var p api.BuildingProfile
	err := a.DB.QueryRow(
		ctx,
		`SELECT floor_area_m2,country_code,address,postal_code,city,tariff_type FROM building_profiles WHERE building_id=$1`,
		r.BuildingId,
	).Scan(
		&p.FloorAreaM2,
		&p.CountryCode,
		&p.Address,
		&p.PostalCode,
		&p.City,
		&p.TariffType,
	)
	return api.GetBuildingProfile200JSONResponse(p), err
}

// SeedCloudOnboarding maps a mock household to existing inventory and issues a separate program invitation.
func (a *App) SeedCloudOnboarding(ctx context.Context, seed local.Seed, subject, code string) error {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO manufacturer_inventory(device_id,provider,subject,external_id) VALUES($1,'demo',$2,$3) ON CONFLICT DO NOTHING`,
		seed.DeviceID,
		subject,
		seed.DeviceID,
	); err != nil {
		return err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO partner_invitations(code_hash,organization_id,expires_at) VALUES($1,$2,now()+interval '7 days') ON CONFLICT(code_hash) DO UPDATE SET expires_at=EXCLUDED.expires_at WHERE partner_invitations.redeemed_by IS NULL AND partner_invitations.organization_id=EXCLUDED.organization_id`,
		identity.Digest(code),
		seed.OrganizationID,
	); err != nil {
		return err
	}
	if err = audit(
		ctx,
		tx,
		seed.OrganizationID,
		seed.UserID,
		"partner_invitation_provisioned",
		seed.DeviceID,
		"local-cli",
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
