//go:build integration

package cloud

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/messaging"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func TestCloudOnboardingAuthorizationAndAtomicOwnership(t *testing.T) {
	db := database(t)
	config := db.Config().Copy()
	config.MaxConns = 1
	single, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	app := &App{DB: single, MQTT: &messaging.Client{}}
	seed := local.Seed{
		OrganizationID: uuid.NewString(),
		UserID:         uuid.NewString(),
		DeviceID:       uuid.NewString(),
		RunID:          uuid.NewString(),
		Login:          "original@example.local",
		Password:       local.Secret(),
		Bootstrap:      local.Secret(),
	}
	if _, err := app.Seed(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	code, subject := local.Secret(), uuid.NewString()
	if err := app.SeedCloudOnboarding(t.Context(), seed, subject, code); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(
		t.Context(),
		`UPDATE partner_invitations SET expires_at=now()-interval '1 day' WHERE code_hash=$1`,
		identity.Digest(code),
	); err != nil {
		t.Fatal(err)
	}
	if err = app.SeedCloudOnboarding(t.Context(), seed, subject, code); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err = db.QueryRow(
		t.Context(),
		`SELECT expires_at>now() FROM partner_invitations WHERE code_hash=$1`,
		identity.Digest(code),
	).Scan(
		&renewed,
	); err != nil ||
		!renewed {
		t.Fatal("unredeemed invitation was not renewed", err)
	}
	secondSeed := seed
	secondSeed.DeviceID, secondSeed.RunID = uuid.NewString(), uuid.NewString()
	if _, err = app.Seed(t.Context(), secondSeed); err != nil {
		t.Fatal(err)
	}
	if err = app.SeedCloudOnboarding(t.Context(), secondSeed, subject, local.Secret()); err != nil {
		t.Fatal(err)
	}
	handler, err := app.Handler(fstest.MapFS{"index.html": {Data: []byte("Thermora")}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	mockConfig := manufacturer.MockConfig{
		Login:        "household@example.local",
		Password:     local.Secret(),
		Subject:      subject,
		ClientSecret: local.Secret(),
		RedirectURL:  server.URL + "/api/v1/manufacturer-authorizations/callback",
		StatePath:    filepath.Join(t.TempDir(), "mock.db"),
		Devices: []manufacturer.Device{
			{ID: seed.DeviceID, Name: "Demo pump"},
			{ID: secondSeed.DeviceID, Name: "Second zone"},
		},
	}
	mock, err := manufacturer.NewMock(mockConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := mock.Close(); e != nil {
			t.Error(e)
		}
	})
	provider := httptest.NewTLSServer(mock.Handler())
	t.Cleanup(provider.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(
		ca,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}
	app.Manufacturer, err = manufacturer.NewClient(
		manufacturer.Config{
			APIURL:        provider.URL,
			AuthorizeURL:  provider.URL + "/oauth/authorize",
			RedirectURL:   mockConfig.RedirectURL,
			ClientSecret:  mockConfig.ClientSecret,
			EncryptionKey: base64.RawStdEncoding.EncodeToString(key),
			CAFile:        ca,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Manufacturer.HTTP.CloseIdleConnections)
	browser := func() *http.Client {
		t.Helper()
		jar, e := cookiejar.New(nil)
		if e != nil {
			t.Fatal(e)
		}
		return &http.Client{
			Transport:     server.Client().Transport,
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	first, second := browser(), browser()
	call := func(client *http.Client, method, path string, body any, key string, status int) []byte {
		t.Helper()
		data, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		if body == nil {
			data = nil
		}
		r, e := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			if strings.HasPrefix(key, "etag:") {
				r.Header.Set("If-Match", strings.TrimPrefix(key, "etag:"))
			} else {
				r.Header.Set("Idempotency-Key", key)
			}
		}
		resp, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		out, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, resp.StatusCode, status, out)
		}
		return out
	}
	var owner api.Session
	if err = json.Unmarshal(
		call(
			first,
			"POST",
			"/api/v1/accounts",
			map[string]string{"login": "New@Example.Local", "password": local.Secret()},
			"",
			201,
		),
		&owner,
	); err != nil {
		t.Fatal(err)
	}
	if owner.Login != "new@example.local" || len(owner.Organizations) != 0 {
		t.Fatal("signup granted unearned tenant access")
	}
	call(
		second,
		"POST",
		"/api/v1/accounts",
		map[string]string{"login": "other@example.local", "password": local.Secret()},
		"",
		201,
	)
	call(
		second,
		"POST",
		"/api/v1/accounts",
		map[string]string{"login": "NEW@example.local", "password": local.Secret()},
		"",
		409,
	)
	var start api.AuthorizeManufacturer201JSONResponse
	if err = json.Unmarshal(
		call(
			first,
			"POST",
			"/api/v1/manufacturer-authorizations",
			map[string]string{"manufacturer_id": "demo"},
			"",
			201,
		),
		&start,
	); err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(start.AuthorizationUrl)
	if err != nil {
		t.Fatal(err)
	}
	callback := "/api/v1/manufacturer-authorizations/callback?state=" + authorize.Query().Get(
		"state",
	) + "&code=not-yet-issued"
	call(second, "GET", callback, nil, "", 400) // No initiating-browser cookie; cannot consume the legitimate flow.
	providerCall := func(method string, form url.Values) (int, *url.URL) {
		t.Helper()
		r, e := http.NewRequestWithContext(
			t.Context(),
			method,
			start.AuthorizationUrl,
			strings.NewReader(form.Encode()),
		)
		if e != nil {
			t.Fatal(e)
		}
		if method == "POST" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, e := first.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.Copy(io.Discard, resp.Body); e != nil {
			t.Fatal(e)
		}
		if e = resp.Body.Close(); e != nil {
			t.Fatal(e)
		}
		location, e := url.Parse(resp.Header.Get("Location"))
		if e != nil {
			t.Fatal(e)
		}
		return resp.StatusCode, location
	}
	if status, _ := providerCall("GET", nil); status != 200 {
		t.Fatalf("authorize page: %d", status)
	}
	var csrf string
	for _, c := range first.Jar.Cookies(authorize) {
		if c.Name == "mock_csrf" {
			csrf = c.Value
		}
	}
	status, location := providerCall(
		"POST",
		url.Values{
			"csrf":     {csrf},
			"login":    {mockConfig.Login},
			"password": {mockConfig.Password},
			"decision": {"allow"},
		},
	)
	if status != 303 {
		t.Fatalf("provider authorization: %d", status)
	}
	callback = location.RequestURI()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, location.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := first.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 303 {
		t.Fatalf("callback: %d", response.StatusCode)
	}
	location, err = response.Location()
	if err != nil {
		t.Fatal(err)
	}
	connection := location.Query().Get("connection")
	if connection == "" {
		t.Fatal("authorization did not create a connection")
	}
	call(first, "GET", callback, nil, "", 400)
	devicesPath := "/api/v1/manufacturer-connections/" + connection + "/devices"
	call(second, "GET", devicesPath, nil, "", 404)
	call(first, "GET", devicesPath, nil, "", 200)
	if _, err = db.Exec(
		t.Context(),
		`UPDATE manufacturer_connections SET expires_at=now() WHERE id=$1`,
		connection,
	); err != nil {
		t.Fatal(err)
	}
	call(first, "GET", devicesPath, nil, "", 200) // Rotating refresh token, real provider HTTP round trip.
	body := api.CloudEnrollment{
		ConnectionId:         connection,
		ManufacturerDeviceId: seed.DeviceID,
		AccessCode:           &code,
		BuildingName:         "Test home",
		Timezone:             "Europe/Helsinki",
		ConsentVersion:       "local-demo-2026-09",
		ConsentAccepted:      false,
		Profile: api.BuildingProfile{
			FloorAreaM2: 120,
			CountryCode: "FI",
			Address:     "Fictional street 1",
			PostalCode:  "00000",
			City:        "Demo city",
			TariffType:  "variable",
		},
	}
	requestKey := uuid.NewString()
	call(first, "POST", "/api/v1/cloud-enrollments", body, requestKey, 422)
	body.ConsentAccepted = true
	call(second, "POST", "/api/v1/cloud-enrollments", body, uuid.NewString(), 404)
	badCode := local.Secret()
	body.AccessCode = &badCode
	call(first, "POST", "/api/v1/cloud-enrollments", body, requestKey, 422)
	body.AccessCode = &code
	var redeemed bool
	if err = db.QueryRow(
		t.Context(),
		`SELECT redeemed_by IS NOT NULL FROM partner_invitations WHERE code_hash=$1`,
		identity.Digest(code),
	).Scan(
		&redeemed,
	); err != nil ||
		redeemed {
		t.Fatal("failed onboarding consumed entitlement", err)
	}
	result := call(first, "POST", "/api/v1/cloud-enrollments", body, requestKey, 201)
	if err = app.SeedCloudOnboarding(t.Context(), seed, subject, code); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(
		t.Context(),
		`SELECT redeemed_by IS NOT NULL FROM partner_invitations WHERE code_hash=$1`,
		identity.Digest(code),
	).Scan(
		&redeemed,
	); err != nil ||
		!redeemed {
		t.Fatal("reseeding reactivated a redeemed invitation", err)
	}
	if same := call(first, "POST", "/api/v1/cloud-enrollments", body, requestKey, 201); !bytes.Equal(result, same) {
		t.Fatal("retry created different ownership")
	}
	var building api.Building
	if err = json.Unmarshal(result, &building); err != nil {
		t.Fatal(err)
	}
	call(first, "GET", "/api/v1/buildings/"+building.Id+"/profile", nil, "", 200)
	call(second, "GET", "/api/v1/buildings/"+building.Id+"/profile", nil, "", 404)
	call(second, "GET", "/api/v1/devices/"+seed.DeviceID, nil, "", 404)
	body.BuildingName = "Changed"
	call(first, "POST", "/api/v1/cloud-enrollments", body, requestKey, 409)
	call(first, "POST", "/api/v1/cloud-enrollments", body, uuid.NewString(), 422)
	var state, consent string
	if err = db.QueryRow(
		t.Context(),
		`SELECT d.enrollment,e.consent_version FROM devices d JOIN cloud_enrollments e ON e.device_id=d.id WHERE d.id=$1`,
		seed.DeviceID,
	).Scan(
		&state,
		&consent,
	); err != nil {
		t.Fatal(err)
	}
	if state != "claimed" || consent != "local-demo-2026-09" {
		t.Fatal("ownership/consent not committed together")
	}
	var secrets []byte
	if err = db.QueryRow(
		t.Context(),
		`SELECT tokens FROM manufacturer_connections WHERE id=$1`,
		connection,
	).Scan(
		&secrets,
	); err != nil {
		t.Fatal(err)
	}
	if json.Valid(secrets) {
		t.Fatal("provider tokens stored as plaintext JSON")
	}
	plaintext, err := app.Manufacturer.Decrypt(secrets, "tokens:"+connection)
	if err != nil {
		t.Fatal(err)
	}
	var tokens manufacturer.Tokens
	if err = json.Unmarshal(plaintext, &tokens); err != nil {
		t.Fatal(err)
	}

	attach := api.AttachManufacturerDeviceJSONBody{
		ConnectionId:         connection,
		ManufacturerDeviceId: secondSeed.DeviceID,
		ZoneName:             "Upstairs",
		ConsentAccepted:      true,
	}
	attachPath := "/api/v1/buildings/" + building.Id + "/manufacturer-enrollments"
	call(second, "POST", attachPath, attach, uuid.NewString(), 404)
	attachKey := uuid.NewString()
	attached := call(first, "POST", attachPath, attach, attachKey, 201)
	if again := call(first, "POST", attachPath, attach, attachKey, 201); !bytes.Equal(attached, again) {
		t.Fatal("attachment retry changed identity")
	}
	profileBody := api.UpdateBuildingProfileJSONBody{Name: "Updated home", Profile: body.Profile}
	profileBody.Profile.TariffType = "fixed"
	profilePath := "/api/v1/buildings/" + building.Id + "/profile"
	call(second, "PUT", profilePath, profileBody, `etag:"1"`, 404)
	call(first, "PUT", profilePath, profileBody, `etag:"1"`, 200)
	call(first, "PUT", profilePath, profileBody, `etag:"1"`, 409)
	var affected int
	if err = db.QueryRow(
		t.Context(),
		`SELECT count(*) FROM devices WHERE building_id=$1 AND (config->>'config_version')::int=2`,
		building.Id,
	).Scan(
		&affected,
	); err != nil ||
		affected != 2 {
		t.Fatal("tariff edit did not fence both zones", affected, err)
	}
	connectionPath := "/api/v1/devices/" + seed.DeviceID + "/connection"
	call(second, "DELETE", connectionPath, nil, "", 404)
	call(first, "DELETE", connectionPath, nil, "", 204)
	allowed, err := connected(t.Context(), db, seed.DeviceID)
	if err != nil || allowed {
		t.Fatal("disconnected device retains authority", err)
	}
	allowed, err = connected(t.Context(), db, secondSeed.DeviceID)
	if err != nil || !allowed {
		t.Fatal("sibling was disconnected", err)
	}
	if _, err = app.manufacturerRoute(t.Context(), seed.DeviceID); err != nil {
		t.Fatal("disconnect erased manufacturer boundary", err)
	}
	call(
		first,
		"PUT",
		connectionPath,
		api.ReconnectDeviceJSONBody{ConnectionId: connection, ConsentAccepted: false},
		"",
		422,
	)
	call(
		first,
		"PUT",
		connectionPath,
		api.ReconnectDeviceJSONBody{ConnectionId: connection, ConsentAccepted: true},
		"",
		202,
	)
	tokens.AccessToken = local.Secret()
	// #nosec G117 -- synthetic fixture token is immediately encrypted before storing the rejection case.
	plaintext, err = json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(
		t.Context(),
		`UPDATE manufacturer_connections SET tokens=$2,expires_at=now()+interval '1 hour' WHERE id=$1`,
		connection,
		app.Manufacturer.Encrypt(plaintext, "tokens:"+connection),
	); err != nil {
		t.Fatal(err)
	}
	call(first, "GET", devicesPath, nil, "", 409)
	if err = db.QueryRow(
		t.Context(),
		`SELECT status FROM manufacturer_connections WHERE id=$1`,
		connection,
	).Scan(
		&state,
	); err != nil ||
		state != "authentication_required" {
		t.Fatal("provider rejection left a connected status", err)
	}

	// Restore valid encrypted credentials, then prove whole-account revocation reaches the provider.
	if _, err = db.Exec(
		t.Context(),
		`UPDATE manufacturer_connections SET tokens=$2,status='connected' WHERE id=$1`,
		connection,
		secrets,
	); err != nil {
		t.Fatal(err)
	}
	call(second, "DELETE", "/api/v1/manufacturer-connections/"+connection, nil, "", 404)
	call(first, "DELETE", "/api/v1/manufacturer-connections/"+connection, nil, "", 204)
	if err = app.revokeManufacturers(t.Context()); err != nil {
		t.Fatal(err)
	}
	var original manufacturer.Tokens
	decoded, err := app.Manufacturer.Decrypt(secrets, "tokens:"+connection)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(decoded, &original); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Manufacturer.Discover(t.Context(), original.AccessToken); err == nil {
		t.Fatal("revocation left the access token usable")
	}
	if _, err = app.Manufacturer.Refresh(t.Context(), original.RefreshToken); err == nil {
		t.Fatal("revocation left the refresh token usable")
	}
	call(
		first,
		"PUT",
		connectionPath,
		api.ReconnectDeviceJSONBody{ConnectionId: connection, ConsentAccepted: true},
		"",
		409,
	)
	provider.Close()
	body.BuildingName = "Test home"
	if same := call(first, "POST", "/api/v1/cloud-enrollments", body, requestKey, 201); !bytes.Equal(result, same) {
		t.Fatal("cached enrollment retry depended on provider availability")
	}
}
