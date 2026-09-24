package manufacturer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	contract "thermora.local/thermora/api"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
)

func TestAuthorizationPKCERefreshAndCredentialIsolation(t *testing.T) {
	cfg := MockConfig{
		OrganizationID: uuid.NewString(),
		Login:          "fixture@example.local",
		Password:       rand.Text(),
		Subject:        "household-one",
		ClientSecret:   rand.Text() + rand.Text(),
		RedirectURL:    "https://cloud.local/callback",
		StatePath:      filepath.Join(t.TempDir(), "mock.db"),
		Devices:        []Device{{ID: uuid.NewString(), Name: "Heat pump"}},
	}
	mock, err := NewMock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := mock.Close(); e != nil {
			t.Error(e)
		}
	})
	server := httptest.NewTLSServer(mock.Handler())
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(
		ca,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(
		Config{
			APIURL:        server.URL,
			AuthorizeURL:  server.URL + "/oauth/authorize",
			RedirectURL:   cfg.RedirectURL,
			ClientSecret:  cfg.ClientSecret,
			EncryptionKey: base64.RawStdEncoding.EncodeToString(key),
			CAFile:        ca,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.HTTP.CloseIdleConnections)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{
		Transport:     server.Client().Transport,
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	verifier, state := rand.Text()+rand.Text(), rand.Text()+rand.Text()
	authorization := client.AuthorizationURL(state, verifier)
	request := func(t *testing.T, method, target string, form url.Values, status int) (*url.URL, http.Header, string) {
		t.Helper()
		r, e := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(form.Encode()))
		if e != nil {
			t.Fatal(e)
		}
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, e := browser.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		if e = resp.Body.Close(); e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s: got HTTP %d, want %d", method, resp.StatusCode, status)
		}
		location, e := url.Parse(resp.Header.Get("Location"))
		if e != nil {
			t.Fatal(e)
		}
		return location, resp.Header, string(body)
	}
	for _, path := range []string{"/", "/?redirect_uri=https://untrusted.invalid/&state=untrusted"} {
		location, _, _ := request(t, "GET", server.URL+path, nil, http.StatusSeeOther)
		if got, want := location.String(), "https://cloud.local/connect"; got != want {
			t.Errorf("GET %s redirects to %q, want %q", path, got, want)
		}
	}
	request(t, "GET", server.URL+"/unknown", nil, http.StatusNotFound)
	request(t, "GET", server.URL+"/oauth/authorize", nil, http.StatusBadRequest)
	for _, asset := range []struct{ path, mediaType string }{
		{path: "/theme/theme.js", mediaType: "text/javascript"},
		{path: "/theme/tokens.css", mediaType: "text/css"},
		{path: "/theme/IBMPlexSans-Regular.woff2", mediaType: "font/woff2"},
		{path: "/theme/IBMPlexSans-SemiBold.woff2", mediaType: "font/woff2"},
	} {
		t.Run(asset.path, func(t *testing.T) {
			_, header, body := request(t, "GET", server.URL+asset.path, nil, http.StatusOK)
			mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			if mediaType != asset.mediaType &&
				(asset.mediaType != "text/javascript" || mediaType != "application/javascript") {
				t.Errorf("GET %s Content-Type = %q, want %q", asset.path, mediaType, asset.mediaType)
			}
			if body == "" {
				t.Errorf("GET %s returned an empty asset", asset.path)
			}
		})
	}
	request(t, "GET", server.URL+"/theme/missing.js", nil, http.StatusNotFound)
	_, header, page := request(t, "GET", authorization, nil, http.StatusOK)
	wantPolicy := "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; " +
		"form-action 'self' https://cloud.local; frame-ancestors 'none'; base-uri 'none'"
	if got := header.Get("Content-Security-Policy"); got != wantPolicy {
		t.Errorf("authorization Content-Security-Policy = %q, want %q", got, wantPolicy)
	}
	u, err := url.Parse(authorization)
	if err != nil {
		t.Fatal(err)
	}
	var csrf string
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "mock_csrf" {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		t.Fatal("authorization did not set a CSRF cookie")
	}
	for _, markup := range []string{
		`<script src="/theme/theme.js"></script>`,
		`<link rel="stylesheet" href="/theme/tokens.css"`,
		`<form method="post">`,
		`<input type="hidden" name="csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(page, markup) {
			t.Errorf("authorization HTML is missing %q", markup)
		}
	}
	form := url.Values{"login": {cfg.Login}, "password": {cfg.Password}, "decision": {"allow"}, "csrf": {"wrong"}}
	request(t, "POST", authorization, form, 403)
	form.Set("csrf", csrf)
	location, _, _ := request(t, "POST", authorization, form, 303)
	if location.Query().Get("state") != state || location.Query().Get("access_token") != "" {
		t.Fatal("invalid authorization redirect")
	}
	code := location.Query().Get("code")
	if _, err = client.Exchange(t.Context(), code, "incorrect-proof"+verifier); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("PKCE mismatch: %v", err)
	}
	tokens, err := client.Exchange(t.Context(), code, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Exchange(t.Context(), code, verifier); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("replayed code accepted: %v", err)
	}
	discovery, err := client.Discover(t.Context(), tokens.AccessToken)
	if err != nil || discovery.Subject != cfg.Subject || len(discovery.Devices) != 1 {
		t.Fatal("device discovery failed", err)
	}
	testEquipment(t, mock, client, tokens.AccessToken, cfg)
	renewed, err := client.Refresh(t.Context(), tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.RefreshToken == tokens.RefreshToken {
		t.Fatal("refresh token did not rotate")
	}
	if _, err = client.Refresh(t.Context(), tokens.RefreshToken); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("refresh replay accepted: %v", err)
	}
	if _, err = client.Discover(t.Context(), "invalid"); !errors.Is(err, ErrAuthorization) {
		t.Fatalf("unauthenticated discovery accepted: %v", err)
	}
	sealed := client.Encrypt([]byte(tokens.RefreshToken), "connection-one")
	if _, err = client.Decrypt(sealed, "connection-two"); err == nil {
		t.Fatal("credential ciphertext accepted for another connection")
	}
	plain, err := client.Decrypt(sealed, "connection-one")
	if err != nil || string(plain) != tokens.RefreshToken {
		t.Fatal("credential round trip failed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = client.Discover(ctx, renewed.AccessToken); !errors.Is(err, context.Canceled) {
		t.Fatalf("provider request ignored cancellation: %v", err)
	}
	bad := *u
	query := bad.Query()
	query.Set("redirect_uri", "https://attacker.invalid")
	bad.RawQuery = query.Encode()
	request(t, "GET", bad.String(), nil, 400)
}

func testEquipment(t *testing.T, mock *Mock, client *Client, token string, cfg MockConfig) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(contract.ManufacturerOpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := cfg.Devices[0].ID
	s := protocol.Sample{
		Sequence:     1,
		Elapsed:      60,
		ObservedAt:   protocol.StartTime.Add(time.Minute),
		RecordedAt:   time.Now().UTC(),
		Indoor:       21,
		Outdoor:      -3,
		Supply:       30,
		RoomTarget:   21,
		SupplyTarget: 40,
		Mode:         "baseline",
		Compressor:   "off",
		Alarms:       []string{},
		Quality:      []string{},
	}
	batch := protocol.Batch{
		SchemaVersion: 1,
		DeviceID:      id,
		RunID:         uuid.NewString(),
		BootID:        uuid.NewString(),
		ID:            uuid.NewString(),
		Origin:        "simulation",
		SentAt:        s.RecordedAt,
		Samples:       []protocol.Sample{s},
	}
	data, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	message := messaging.Message{Topic: protocol.Topic(cfg.OrganizationID, id, "telemetry"), Payload: data}
	for range 2 {
		if err = mock.record(message); err != nil {
			t.Fatal(err)
		}
	}
	state, err := client.State(t.Context(), token, id)
	if err != nil || len(state.Messages) != 1 || state.Latest == nil || state.Latest.Sequence != 1 {
		t.Fatal("evidence not retained/deduplicated", err)
	}
	if _, err = client.State(t.Context(), token, uuid.NewString()); err == nil {
		t.Fatal("cross-household read accepted")
	}
	if _, err = client.State(t.Context(), "invalid", id); !errors.Is(err, ErrAuthorization) {
		t.Fatal("unauthenticated state accepted", err)
	}
	for range 2 {
		if err = client.Receipt(t.Context(), token, id, state.Messages[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = mock.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("equipment:"+id)).Stats().KeyN != 0 {
			t.Error("acknowledged inbox entry retained")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Cache may repeat the last immutable sample; it must not manufacture a new recording time.
	repeated, err := client.State(t.Context(), token, id)
	if err != nil || repeated.Latest == nil || !repeated.Latest.RecordedAt.Equal(s.RecordedAt) {
		t.Fatal("cached evidence gained false freshness", err)
	}
	configuration := protocol.NewConfiguration(id, batch.RunID, protocol.Comfort{Target: 21, Min: 20, Max: 22})
	command := protocol.Command{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		DeviceID:      id,
		RunID:         batch.RunID,
		RunEpoch:      1,
		Sequence:      1,
		Generation:    1,
		ConfigVersion: 1,
		IssuedAt:      time.Now().UTC(),
		ExpiresAt:     time.Now().Add(30 * time.Second),
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(time.Hour),
		Kind:          "set_heating_policy",
		Parameters:    protocol.Parameters{Mode: "remote", Room: 22, Supply: 42},
	}
	command.ExpiresAt = command.IssuedAt.Add(30 * time.Second)
	encoded, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateControl(id, Message{Kind: "command", Payload: encoded}); err != nil {
		t.Fatal("valid instruction rejected", err)
	}
	command.ExpiresAt = time.Now().Add(-time.Second)
	expired, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateControl(id, Message{Kind: "command", Payload: expired}); err == nil {
		t.Fatal("expired instruction accepted")
	}
	configuration.WeatherProfile = 99
	invalid, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateControl(id, Message{Kind: "config/desired", Payload: invalid}); err == nil {
		t.Fatal("unsupported scenario accepted")
	}
	if err = decodeEnvelope(strings.NewReader(`{"id":"a"} {}`), new(Message)); err == nil {
		t.Fatal("trailing envelope accepted")
	}
	readOnly := rand.Text()
	if err = mock.db.Update(func(tx *bolt.Tx) error {
		return putGrant(tx, readOnly, grant{Kind: "access", Expires: time.Now().Add(time.Hour), Scope: "devices:read"})
	}); err != nil {
		t.Fatal(err)
	}
	if err = client.Send(t.Context(), readOnly, id, "command", encoded); !errors.Is(err, ErrAuthorization) {
		t.Fatal("read-only scope permitted a write", err)
	}
	message.Topic = protocol.Topic(uuid.NewString(), id, "telemetry")
	if err = mock.record(message); err == nil {
		t.Fatal("wrong-tenant MQTT evidence accepted")
	}
}
