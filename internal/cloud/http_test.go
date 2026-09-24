package cloud

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	contract "thermora.local/thermora/api"
)

func TestContractRejectsInvalidLoginBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()
	app := &App{}
	handler, err := app.Handler(fstest.MapFS{"index.html": {Data: []byte("Thermora")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, origin string
		status             int
	}{
		{"missing password", `{"login":"owner"}`, "", 400},
		{"unknown fields", `{"login":"owner","password":"password","admin":true}`, "", 400},
		{"trailing JSON", `{"login":"owner","password":"password"} {}`, "", 400},
		{"cross-origin", `{"login":"owner","password":"password"}`, "https://attacker.invalid", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"https://localhost:8443/api/v1/session",
				strings.NewReader(tc.body),
			)
			r.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAPIExplorer(t *testing.T) {
	t.Parallel()
	app := &App{}
	handler, err := app.Handler(fstest.MapFS{
		"index.html":      {Data: []byte("dashboard")},
		"docs/index.html": {Data: []byte("API explorer")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/openapi.yaml", http.StatusOK},
		{http.MethodHead, "/openapi.yaml", http.StatusOK},
		{http.MethodPost, "/openapi.yaml", http.StatusMethodNotAllowed},
		{http.MethodGet, "/docs/", http.StatusOK},
		{http.MethodGet, "/docs", http.StatusTemporaryRedirect},
		{http.MethodGet, "/health/live", http.StatusNoContent},
		{http.MethodGet, "/health/ready", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/v1/session", http.StatusUnauthorized},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), tc.method, "https://localhost:8443"+tc.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing content security policy")
			}
			if tc.path == "/openapi.yaml" && tc.status == http.StatusOK {
				if w.Header().Get("Content-Type") != "application/yaml" ||
					w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("incorrect spec headers: %v", w.Header())
				}
				if tc.method == http.MethodGet && !bytes.Equal(w.Body.Bytes(), contract.OpenAPI) {
					t.Fatal("served contract differs from the authoritative source")
				}
				if tc.method == http.MethodHead && w.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
			}
			if tc.path == "/docs/" && w.Body.String() != "API explorer" {
				t.Fatalf("served dashboard instead of explorer: %s", w.Body.String())
			}
			if tc.path == "/docs" && w.Header().Get("Location") != "/docs/" {
				t.Fatalf("incorrect redirect: %q", w.Header().Get("Location"))
			}
		})
	}
}
