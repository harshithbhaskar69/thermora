// Package cloud hosts the modular application: identity, ownership, ingestion, and control.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	contract "thermora.local/thermora/api"
	"thermora.local/thermora/internal/artifacts"
	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/messaging"
	api "thermora.local/thermora/internal/transport/http/generated"
)

type App struct {
	// ponytail: one application replica; this fence serializes control mutations against external delivery. Use database-backed delivery leases before adding replicas.
	controlMu    sync.RWMutex
	Artifacts    *artifacts.Store
	DB           *pgxpool.Pool
	MQTT         *messaging.Client
	Admin        *messaging.Admin
	Manufacturer *manufacturer.Client
	loginMu      sync.Mutex
	loginWindow  time.Time
	loginCount   int
}

type (
	requestKey   struct{}
	requestState struct {
		writer            http.ResponseWriter
		request           *http.Request
		userID, login, id string
	}
)

func request(ctx context.Context) *requestState {
	state, ok := ctx.Value(requestKey{}).(*requestState)
	if !ok || state == nil {
		panic("cloud handler called without request middleware")
	}
	return state
}

type problem struct {
	status       int
	code, detail string
}

func (p *problem) Error() string                 { return p.code }
func fail(status int, code, detail string) error { return &problem{status, code, detail} }
func missing() error                             { return fail(404, "not_found", "The resource is unavailable.") }

func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	p := &problem{500, "internal_error", "The request could not be completed."}
	if errors.Is(err, pgx.ErrNoRows) {
		p = &problem{404, "not_found", "The resource is unavailable."}
	} else {
		var known *problem
		if errors.As(err, &known) {
			p = known
		} else {
			slog.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "error", err)
		}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.status)
	_ = json.NewEncoder(w).Encode(api.Problem{
		Type:      "urn:thermora:problem:" + p.code,
		Title:     http.StatusText(p.status),
		Status:    p.status,
		Code:      p.code,
		Detail:    p.detail,
		RequestId: w.Header().Get("X-Request-ID"),
	})
}

func (a *App) Handler(assets fs.FS) (http.Handler, error) {
	spec, err := api.GetSpec()
	if err != nil {
		return nil, err
	}
	spec.Servers = nil
	if err = spec.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("validate HTTP contract: %w", err)
	}
	router, err := legacy.NewRouter(spec)
	if err != nil {
		return nil, err
	}
	strict := api.NewStrictHandlerWithOptions(a, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, fail(400, "invalid_request", "The request does not match the API contract."))
		},
		ResponseErrorHandlerFunc: writeProblem,
	})
	generated := api.HandlerWithOptions(
		strict,
		api.StdHTTPServerOptions{ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, fail(400, "invalid_request", "Invalid route parameters."))
		}},
	)
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := request(r.Context())
		public := r.Method == http.MethodPost &&
			(r.URL.Path == "/api/v1/session" || r.URL.Path == "/api/v1/accounts" || r.URL.Path == "/edge/v1/enroll") ||
			r.Method == http.MethodGet && r.URL.Path == "/api/v1/manufacturer-authorizations/callback"
		if !public {
			cookie, err := r.Cookie("thermora_session")
			if err != nil {
				writeProblem(w, r, fail(401, "unauthenticated", "Sign in to continue."))
				return
			}
			err = a.DB.QueryRow(
				r.Context(),
				`SELECT u.id,u.login FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now()`,
				identity.Digest(cookie.Value),
			).Scan(
				&state.userID,
				&state.login,
			)
			if errors.Is(err, pgx.ErrNoRows) {
				writeProblem(w, r, fail(401, "unauthenticated", "Sign in to continue."))
				return
			}
			if err != nil {
				writeProblem(w, r, err)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil || (len(body) > 0 && !json.Valid(body)) {
			writeProblem(w, r, fail(400, "invalid_request", "Provide one JSON value within the 64 KiB request limit."))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		route, params, err := router.FindRoute(r)
		if err != nil {
			writeProblem(w, r, missing())
			return
		}
		err = openapi3filter.ValidateRequest(
			r.Context(),
			&openapi3filter.RequestValidationInput{
				Request:    r,
				PathParams: params,
				Route:      route,
				Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
		)
		if err != nil {
			writeProblem(w, r, fail(400, "invalid_request", "The request does not match the API contract."))
			return
		}
		generated.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/edge/", apiHandler)
	mux.Handle("/health/", generated)
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		http.ServeContent(w, r, "openapi.yaml", time.Time{}, bytes.NewReader(contract.OpenAPI))
	})
	mux.HandleFunc("GET /manufacturer-openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		http.ServeContent(w, r, "manufacturer-openapi.yaml", time.Time{}, bytes.NewReader(contract.ManufacturerOpenAPI))
	})
	mux.HandleFunc("GET /docs/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "docs/index.html")
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeProblem(w, r, fail(405, "method_not_allowed", "This page is read-only."))
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(assets, path); err != nil {
			path = "index.html"
		}
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		// #nosec G703 -- assets is the immutable embedded SPA filesystem, never the host filesystem.
		http.ServeFileFS(w, r, assets, path)
	}))
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
		)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		state := &requestState{writer: w, request: r, id: id}
		ctx = context.WithValue(ctx, requestKey{}, state)
		protected.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func (a *App) GetLiveness(context.Context, api.GetLivenessRequestObject) (api.GetLivenessResponseObject, error) {
	return api.GetLiveness204Response{}, nil
}

func (a *App) GetReadiness(
	ctx context.Context,
	_ api.GetReadinessRequestObject,
) (api.GetReadinessResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if a.DB == nil || a.MQTT == nil {
		return api.GetReadiness503Response{}, nil
	}
	if err := a.DB.Ping(ctx); err != nil || !a.MQTT.Ready.Load() {
		return api.GetReadiness503Response{}, nil
	}
	return api.GetReadiness204Response{}, nil
}
