package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

var dummyPasswordHash = identity.HashPassword("unused-local-account")

func (a *App) allowAccountAttempt() bool {
	a.loginMu.Lock()
	if time.Since(a.loginWindow) > time.Minute {
		a.loginWindow = time.Now()
		a.loginCount = 0
	}
	a.loginCount++
	allowed := a.loginCount <= 20
	a.loginMu.Unlock()
	// ponytail: one bounded login budget for this local instance; use per-IP/account limits before public hosting.
	return allowed
}

func (a *App) Login(ctx context.Context, r api.LoginRequestObject) (api.LoginResponseObject, error) {
	if !a.allowAccountAttempt() {
		return nil, fail(429, "login_rate_limited", "Wait a minute before signing in again.")
	}
	var id, hash string
	err := a.DB.QueryRow(
		ctx,
		`SELECT id,password_hash FROM users WHERE login=$1`,
		strings.ToLower(strings.TrimSpace(r.Body.Login)),
	).Scan(
		&id,
		&hash,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		hash = dummyPasswordHash
	}
	valid := identity.CheckPassword(hash, r.Body.Password)
	if !valid || id == "" {
		return nil, fail(401, "invalid_credentials", "The login or password is incorrect.")
	}
	token := local.Secret()
	if _, err = a.DB.Exec(
		ctx,
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '8 hours')`,
		identity.Digest(token),
		id,
	); err != nil {
		return nil, err
	}
	setSession(ctx, id, strings.ToLower(strings.TrimSpace(r.Body.Login)), token)
	session, err := a.session(ctx)
	return api.Login200JSONResponse(session), err
}

func setSession(ctx context.Context, id, login, token string) {
	s := request(ctx)
	s.userID, s.login = id, login
	http.SetCookie(
		s.writer,
		&http.Cookie{
			Name:     "thermora_session",
			Value:    token,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   8 * 3600,
		},
	)
}

func (a *App) RegisterAccount(
	ctx context.Context,
	r api.RegisterAccountRequestObject,
) (api.RegisterAccountResponseObject, error) {
	if !a.allowAccountAttempt() {
		return nil, fail(429, "account_rate_limited", "Wait a minute before trying again.")
	}
	login := strings.ToLower(strings.TrimSpace(r.Body.Login))
	address, err := mail.ParseAddress(login)
	if err != nil || address.Address != login || !strings.Contains(login, "@") {
		return nil, fail(422, "invalid_login", "Enter a valid email address.")
	}
	hash := identity.HashPassword(r.Body.Password)
	id, token := uuid.NewString(), local.Secret()
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO users(id,login,password_hash) VALUES($1,$2,$3)`,
		id,
		login,
		hash,
	); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return nil, fail(409, "account_unavailable", "Unable to create this account. Try signing in.")
		}
		return nil, err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '8 hours')`,
		identity.Digest(token),
		id,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	setSession(ctx, id, login, token)
	return api.RegisterAccount201JSONResponse(
		api.Session{UserId: id, Login: login, Organizations: []api.Organization{}},
	), nil
}

func (a *App) session(ctx context.Context) (api.Session, error) {
	s := request(ctx)
	orgs, err := a.organizations(ctx, s.userID)
	return api.Session{UserId: s.userID, Login: s.login, Organizations: orgs}, err
}

func (a *App) GetSession(ctx context.Context, _ api.GetSessionRequestObject) (api.GetSessionResponseObject, error) {
	s, err := a.session(ctx)
	return api.GetSession200JSONResponse(s), err
}

func (a *App) Logout(ctx context.Context, _ api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	s := request(ctx)
	cookie, err := s.request.Cookie("thermora_session")
	if err != nil {
		return nil, err
	}
	if _, err = a.DB.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, identity.Digest(cookie.Value)); err != nil {
		return nil, err
	}
	http.SetCookie(
		s.writer,
		&http.Cookie{
			Name:     "thermora_session",
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   -1,
		},
	)
	return api.Logout204Response{}, nil
}

func (a *App) organizations(ctx context.Context, user string) ([]api.Organization, error) {
	rows, err := a.DB.Query(
		ctx,
		`SELECT o.id,o.name,m.role FROM organizations o JOIN memberships m ON m.organization_id=o.id WHERE m.user_id=$1 ORDER BY o.name LIMIT 200`,
		user,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []api.Organization{}
	for rows.Next() {
		var o api.Organization
		if err = rows.Scan(&o.Id, &o.Name, &o.Role); err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

func (a *App) ListOrganizations(
	ctx context.Context,
	_ api.ListOrganizationsRequestObject,
) (api.ListOrganizationsResponseObject, error) {
	r, err := a.organizations(ctx, request(ctx).userID)
	return api.ListOrganizations200JSONResponse(r), err
}

// Seed creates application identities separately from local certificates and infrastructure.
func (a *App) Seed(ctx context.Context, s local.Seed) (string, error) {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO organizations(id,name) VALUES($1,'Thermora Demo') ON CONFLICT DO NOTHING`,
		s.OrganizationID,
	); err != nil {
		return "", err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO users(id,login,password_hash) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
		s.UserID,
		s.Login,
		identity.HashPassword(s.Password),
	); err != nil {
		return "", err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO memberships(organization_id,user_id,role) VALUES($1,$2,'owner') ON CONFLICT DO NOTHING`,
		s.OrganizationID,
		s.UserID,
	); err != nil {
		return "", err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO devices(id,organization_id,bootstrap_hash,bootstrap_expires_at,run_id,run_epoch) VALUES($1,$2,$3,now()+interval '24 hours',$4,1) ON CONFLICT DO NOTHING`,
		s.DeviceID,
		s.OrganizationID,
		identity.Digest(s.Bootstrap),
		s.RunID,
	); err != nil {
		return "", err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO simulation_runs(id,device_id,epoch) SELECT run_id,id,run_epoch FROM devices WHERE id=$1 ON CONFLICT(id) DO NOTHING`,
		s.DeviceID,
	); err != nil {
		return "", err
	}
	claim := local.Secret()
	tag, err := tx.Exec(
		ctx,
		`UPDATE devices SET claim_hash=$2,claim_expires_at=now()+interval '10 minutes',bootstrap_expires_at=now()+interval '24 hours' WHERE id=$1 AND enrollment='inventory'`,
		s.DeviceID,
		identity.Digest(claim),
	)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		claim = ""
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO audit(organization_id,actor_id,action,resource_id,request_id) VALUES($1,$2,'demo_seed',$3,'local-cli')`,
		s.OrganizationID,
		s.UserID,
		s.DeviceID,
	); err != nil {
		return "", err
	}
	return claim, tx.Commit(ctx)
}

// SeedOperator is available only through the local administrative CLI, never public signup.
func (a *App) SeedOperator(ctx context.Context, account local.OperatorAccess) error {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer storage.Rollback(ctx, tx)
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO users(id,login,password_hash) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`,
		account.UserID,
		account.Login,
		identity.HashPassword(account.Password),
	); err != nil {
		return err
	}
	if _, err = tx.Exec(
		ctx,
		`INSERT INTO memberships(organization_id,user_id,role) VALUES($1,$2,'operator') ON CONFLICT DO NOTHING`,
		account.OrganizationID,
		account.UserID,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
