package manufacturer

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/web"
)

// MockConfig describes one explicitly provisioned demonstration household.
// This is a local authorization/discovery fixture, not an OEM equipment cloud.
type MockConfig struct {
	Login, Password, Subject, ClientSecret, RedirectURL, Certificate, Key, StatePath string
	Devices                                                                          []Device
	OrganizationID                                                                   string
	MQTT                                                                             messaging.Settings
}

type Mock struct {
	grantMu        sync.RWMutex
	config         MockConfig
	passwordHash   string
	redirectOrigin string
	db             *bolt.DB
	mu             sync.Mutex
	window         time.Time
	attempts       int
	mqtt           *messaging.Client
}

type grant struct {
	Kind      string    `json:"kind"`
	Family    string    `json:"family,omitempty"`
	Challenge string    `json:"challenge,omitempty"`
	Expires   time.Time `json:"expires"`
	Scope     string    `json:"scope,omitempty"`
}

func NewMock(cfg MockConfig) (*Mock, error) {
	if cfg.Subject == "" || len(cfg.Devices) == 0 || len(cfg.Devices) > 25 || len(cfg.ClientSecret) < 32 ||
		len(cfg.Password) < 12 {
		return nil, errors.New("invalid mock manufacturer configuration")
	}
	u, err := url.Parse(cfg.RedirectURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid mock redirect URL")
	}
	db, err := bolt.Open(cfg.StatePath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err = db.Update(
		func(tx *bolt.Tx) error { _, e := tx.CreateBucketIfNotExists([]byte("grants")); return e },
	); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Mock{
		config:         cfg,
		passwordHash:   identity.HashPassword(cfg.Password),
		redirectOrigin: u.Scheme + "://" + u.Host,
		db:             db,
	}, nil
}

func (m *Mock) Close() error { return m.db.Close() }

func (m *Mock) Handler() http.Handler {
	theme, err := web.ThemeAssets()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Theme assets are unavailable.", http.StatusInternalServerError)
		})
	}

	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.RedirectHandler(m.redirectOrigin+"/connect", http.StatusSeeOther))
	mux.Handle("GET /assets/", http.FileServer(http.FS(authorizationAssets)))
	mux.Handle("GET /theme/", http.FileServer(http.FS(theme)))
	mux.HandleFunc("GET /oauth/authorize", m.authorize)
	mux.HandleFunc("POST /oauth/authorize", m.authorize)
	mux.HandleFunc("POST /oauth/token", m.token)
	mux.HandleFunc("POST /oauth/revoke", m.revoke)
	mux.HandleFunc("GET /devices", m.devices)
	mux.HandleFunc("GET /devices/{device}/state", m.equipmentState)
	mux.HandleFunc("POST /devices/{device}/control", m.equipmentControl)
	mux.HandleFunc("POST /devices/{device}/receipts", m.equipmentReceipt)
	mux.HandleFunc(
		"GET /health/live",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	)
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Chromium also checks form-action on the OAuth POST's redirect chain.
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; "+
				"form-action 'self' "+m.redirectOrigin+"; frame-ancestors 'none'; base-uri 'none'",
		)
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		protected.ServeHTTP(w, r)
	})
}

//go:embed assets/authorization.css assets/authorization.html
var authorizationAssets embed.FS

var authorizationPage = template.Must(template.ParseFS(authorizationAssets, "assets/authorization.html"))

func (m *Mock) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("redirect_uri") != m.config.RedirectURL ||
		q.Get("response_type") != "code" ||
		q.Get("scope") != "devices:read devices:control" ||
		q.Get("code_challenge_method") != "S256" ||
		len(q.Get("code_challenge")) != 43 ||
		len(q.Get("state")) < 32 ||
		len(q.Get("state")) > 128 {
		http.Error(w, "Invalid authorization request.", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		csrf := rand.Text() + rand.Text()
		http.SetCookie(
			w,
			&http.Cookie{
				Name:     "mock_csrf",
				Value:    csrf,
				Path:     "/oauth/authorize",
				Secure:   true,
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   300,
			},
		)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = authorizationPage.Execute(w, csrf)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", 400)
		return
	}
	cookie, err := r.Cookie("mock_csrf")
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.PostForm.Get("csrf"))) != 1 {
		http.Error(w, "Restart authorization.", http.StatusForbidden)
		return
	}
	redirect := url.Values{"state": {q.Get("state")}}
	if r.PostForm.Get("decision") == "deny" {
		redirect.Set("error", "access_denied")
		http.Redirect(w, r, m.config.RedirectURL+"?"+redirect.Encode(), http.StatusSeeOther)
		return
	}
	m.mu.Lock()
	if time.Since(m.window) > time.Minute {
		m.window, m.attempts = time.Now(), 0
	}
	m.attempts++
	allowed := m.attempts <= 20
	m.mu.Unlock()
	// ponytail: one login budget for the single fixture household; use account/IP budgets for a multi-user provider.
	if !allowed {
		http.Error(w, "Wait a minute before retrying.", http.StatusTooManyRequests)
		return
	}
	if r.PostForm.Get("login") != m.config.Login ||
		!identity.CheckPassword(m.passwordHash, r.PostForm.Get("password")) {
		http.Error(w, "Incorrect manufacturer credentials. Go back to retry.", http.StatusUnauthorized)
		return
	}
	code := rand.Text() + rand.Text()
	err = m.db.Update(func(tx *bolt.Tx) error {
		return putGrant(
			tx,
			code,
			grant{
				Kind:      "code",
				Challenge: q.Get("code_challenge"),
				Expires:   time.Now().Add(time.Minute),
				Scope:     q.Get("scope"),
			},
		)
	})
	if err != nil {
		http.Error(w, "Authorization unavailable.", http.StatusServiceUnavailable)
		return
	}
	redirect.Set("code", code)
	http.Redirect(w, r, m.config.RedirectURL+"?"+redirect.Encode(), http.StatusSeeOther)
}

func putGrant(tx *bolt.Tx, token string, value grant) error {
	b := tx.Bucket([]byte("grants"))
	cursor := b.Cursor()
	count := 0
	for key, data := cursor.First(); key != nil; key, data = cursor.Next() {
		var g grant
		if err := json.Unmarshal(data, &g); err != nil {
			return err
		}
		if time.Now().After(g.Expires) {
			if err := cursor.Delete(); err != nil {
				return err
			}
		} else {
			count++
		}
	}
	if count >= 1024 {
		return errors.New("mock grant limit reached")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.Put(identity.Digest(token), data)
}

func readGrant(tx *bolt.Tx, token, kind string) (grant, error) {
	var g grant
	data := tx.Bucket([]byte("grants")).Get(identity.Digest(token))
	if len(data) == 0 {
		return g, ErrAuthorization
	}
	if err := json.Unmarshal(data, &g); err != nil {
		return g, err
	}
	if g.Kind != kind || !time.Now().Before(g.Expires) {
		return g, ErrAuthorization
	}
	return g, nil
}

func (m *Mock) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(m.config.ClientSecret)) != 1 {
		http.Error(w, "Invalid client.", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", 400)
		return
	}
	kind, token := "code", r.PostForm.Get("code")
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if r.PostForm.Get("redirect_uri") != m.config.RedirectURL {
			http.Error(w, "Invalid redirect.", 400)
			return
		}
	case "refresh_token":
		kind, token = "refresh", r.PostForm.Get("refresh_token")
	default:
		http.Error(w, "Unsupported grant.", 400)
		return
	}
	result := Tokens{
		AccessToken:  rand.Text() + rand.Text(),
		RefreshToken: rand.Text() + rand.Text(),
		TokenType:    "Bearer",
		ExpiresIn:    120,
	}
	err := m.db.Update(func(tx *bolt.Tx) error {
		g, err := readGrant(tx, token, kind)
		if err != nil {
			return err
		}
		if kind == "code" &&
			(len(r.PostForm.Get("code_verifier")) < 43 || Challenge(r.PostForm.Get("code_verifier")) != g.Challenge) {
			return ErrAuthorization
		}
		if err = tx.Bucket([]byte("grants")).Delete(identity.Digest(token)); err != nil {
			return err
		}
		if g.Family == "" {
			g.Family = rand.Text()
		}
		if err = putGrant(
			tx,
			result.AccessToken,
			grant{Family: g.Family, Kind: "access", Expires: time.Now().Add(120 * time.Second), Scope: g.Scope},
		); err != nil {
			return err
		}
		return putGrant(
			tx,
			result.RefreshToken,
			grant{Family: g.Family, Kind: "refresh", Expires: time.Now().Add(24 * time.Hour), Scope: g.Scope},
		)
	})
	if errors.Is(err, ErrAuthorization) {
		http.Error(w, "Invalid or expired grant.", 400)
		return
	}
	if err != nil {
		http.Error(w, "Token service unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// #nosec G117 -- this TLS OAuth endpoint intentionally delivers credentials only to the authenticated confidential client.
	_ = json.NewEncoder(w).Encode(result)
}

func (m *Mock) devices(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "Authorization required.", http.StatusUnauthorized)
		return
	}
	err := m.db.View(func(tx *bolt.Tx) error { _, e := readGrant(tx, token, "access"); return e })
	if err != nil {
		http.Error(w, "Authorization required.", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Discovery{Subject: m.config.Subject, Devices: m.config.Devices})
}

// revoke invalidates the refresh family, including already issued access tokens.
func (m *Mock) revoke(w http.ResponseWriter, r *http.Request) {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	id, secret, ok := r.BasicAuth()
	if !ok || id != ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(m.config.ClientSecret)) != 1 {
		http.Error(w, "Invalid client.", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", 400)
		return
	}
	err := m.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("grants"))
		token := r.PostForm.Get("token")
		raw := bucket.Get(identity.Digest(token))
		if raw == nil {
			return nil
		}
		var current grant
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if current.Family == "" {
			return bucket.Delete(identity.Digest(token))
		}
		cursor := bucket.Cursor()
		for key, data := cursor.First(); key != nil; key, data = cursor.Next() {
			var g grant
			if err := json.Unmarshal(data, &g); err != nil {
				return err
			}
			if g.Family == current.Family {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "Revocation unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
