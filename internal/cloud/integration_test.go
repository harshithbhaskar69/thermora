//go:build integration

package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	"thermora.local/thermora/internal/testutil"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	password := local.Secret()
	env := filepath.Join(t.TempDir(), "postgres.env")
	if err := os.WriteFile(
		env,
		fmt.Appendf(nil, "POSTGRES_USER=thermora\nPOSTGRES_DB=thermora\nPOSTGRES_PASSWORD=%s\n", password),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	name := testutil.Container(
		t,
		"--env-file",
		env,
		"-p",
		"127.0.0.1::5432",
		"postgres:18.6-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873",
	)
	connection := "postgres://thermora:" + url.QueryEscape(
		password,
	) + "@" + testutil.Port(
		t,
		name,
		"5432",
	) + "/thermora?sslmode=disable"
	var db *pgxpool.Pool
	var err error
	deadline := time.Now().Add(20 * time.Second)
	for {
		db, err = storage.Open(t.Context(), connection)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(db.Close)
	if err = storage.Migrate(t.Context(), connection); err != nil {
		t.Fatal(err)
	}
	appPassword := local.Secret() + "'quote"
	if err = storage.ProvisionAppRole(t.Context(), connection, appPassword); err != nil {
		t.Fatal(err)
	}
	appURL, err := url.Parse(connection)
	if err != nil {
		t.Fatal(err)
	}
	appURL.User = url.UserPassword("thermora_app", appPassword)
	app, err := storage.Open(t.Context(), appURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func TestRollbackAfterCancellationKeepsConnectionUsable(t *testing.T) {
	db := database(t)
	conn, err := db.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	storage.Rollback(ctx, tx)
	if _, err = tx.Exec(t.Context(), "SELECT 1"); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("transaction was not closed: %v", err)
	}
	if _, err = conn.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatalf("cancelled cleanup discarded the connection: %v", err)
	}
	storage.Rollback(ctx, tx) // Repeated cleanup must also tolerate a closed transaction.
}

func TestTenantOwnershipIdempotencyAndOutbox(t *testing.T) {
	db := database(t)
	for _, statement := range []string{
		`CREATE TABLE public.forbidden(id integer)`,
		`SELECT * FROM goose_db_version`,
		`ALTER ROLE thermora_app SUPERUSER`,
	} {
		if _, err := db.Exec(t.Context(), statement); err == nil {
			t.Fatalf("application identity accepted privileged operation: %s", statement)
		}
	}
	app := &App{DB: db, MQTT: &messaging.Client{}}
	owner := local.Seed{
		OrganizationID: uuid.NewString(),
		UserID:         uuid.NewString(),
		DeviceID:       uuid.NewString(),
		RunID:          uuid.NewString(),
		Login:          "owner@one.local",
		Password:       local.Secret(),
		Bootstrap:      local.Secret(),
	}
	other := local.Seed{
		OrganizationID: uuid.NewString(),
		UserID:         uuid.NewString(),
		DeviceID:       uuid.NewString(),
		RunID:          uuid.NewString(),
		Login:          "owner@two.local",
		Password:       local.Secret(),
		Bootstrap:      local.Secret(),
	}
	claim, err := app.Seed(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.Seed(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	handler, err := app.Handler(fstest.MapFS{"index.html": {Data: []byte("Thermora")}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	call := func(client *http.Client, method, path string, body any, key string, status int) []byte {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if body == nil {
			data = nil
		}
		r, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			if value, ok := strings.CutPrefix(key, "etag:"); ok {
				r.Header.Set("If-Match", value)
			} else {
				r.Header.Set("Idempotency-Key", key)
			}
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		var raw json.RawMessage
		if resp.StatusCode != 204 {
			if err = json.NewDecoder(resp.Body).Decode(&raw); err != nil {
				t.Fatal(err)
			}
		}
		if resp.StatusCode != status {
			t.Fatalf("%s %s: status=%d expected=%d body=%s", method, path, resp.StatusCode, status, raw)
		}
		return raw
	}
	login := func(s local.Seed) *http.Client {
		t.Helper()
		client := server.Client()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client = &http.Client{Transport: client.Transport, Jar: jar}
		call(client, "POST", "/api/v1/session", map[string]string{"login": s.Login, "password": s.Password}, "", 200)
		return client
	}
	first, second := login(owner), login(other)
	key := uuid.NewString()
	body := map[string]string{"name": "Test building", "timezone": "Europe/Helsinki"}
	path := "/api/v1/organizations/" + owner.OrganizationID + "/buildings"
	result := call(first, "POST", path, body, key, 201)
	same := call(first, "POST", path, body, key, 201)
	if !bytes.Equal(result, same) {
		t.Fatal("idempotent create changed response")
	}
	call(first, "POST", path, map[string]string{"name": "Different", "timezone": "Europe/Helsinki"}, key, 409)
	var building api.Building
	if err = json.Unmarshal(result, &building); err != nil {
		t.Fatal(err)
	}
	call(second, "GET", "/api/v1/buildings/"+building.Id, nil, "", 404)
	call(second, "POST", path, body, uuid.NewString(), 404)
	call(
		first,
		"POST",
		"/api/v1/buildings/"+building.Id+"/enrollments",
		map[string]string{"claim_code": local.Secret()},
		uuid.NewString(),
		422,
	)
	call(
		first,
		"POST",
		"/api/v1/buildings/"+building.Id+"/enrollments",
		map[string]string{"claim_code": claim},
		uuid.NewString(),
		202,
	)
	if next, seedErr := app.Seed(t.Context(), owner); seedErr != nil || next != "" {
		t.Fatal("reseeding a claimed heat pump must not issue another claim", seedErr)
	}
	additional := owner
	additional.DeviceID, additional.RunID, additional.Bootstrap = uuid.NewString(), uuid.NewString(), local.Secret()
	additionalClaim, err := app.Seed(t.Context(), additional)
	if err != nil {
		t.Fatal(err)
	}
	var secondBuilding api.Building
	if err = json.Unmarshal(
		call(
			first,
			"POST",
			path,
			map[string]string{"name": "Second building", "timezone": "Europe/Helsinki"},
			uuid.NewString(),
			201,
		),
		&secondBuilding,
	); err != nil {
		t.Fatal(err)
	}
	enrollPath := "/api/v1/buildings/" + secondBuilding.Id + "/enrollments"
	call(first, "POST", enrollPath, map[string]string{"claim_code": owner.DeviceID}, uuid.NewString(), 422)
	call(first, "POST", enrollPath, map[string]string{"claim_code": claim}, uuid.NewString(), 422)
	call(first, "POST", enrollPath, map[string]string{"claim_code": additionalClaim}, uuid.NewString(), 202)
	for _, pair := range []struct{ device, building string }{{owner.DeviceID, building.Id}, {additional.DeviceID, secondBuilding.Id}} {
		var device api.Device
		if err = json.Unmarshal(
			call(first, "GET", "/api/v1/devices/"+pair.device, nil, "", 200),
			&device,
		); err != nil ||
			device.BuildingId != pair.building {
			t.Fatal("claiming a second pump changed device ownership", err)
		}
	}
	for _, suffix := range []string{"", "/series", "/commands", "/optimization", "/comparisons"} {
		call(second, "GET", "/api/v1/devices/"+owner.DeviceID+suffix, nil, "", 404)
	}
	call(
		second,
		"POST",
		"/api/v1/devices/"+owner.DeviceID+"/overrides",
		map[string]any{"room_target_c": 22, "duration_sim_minutes": 30},
		uuid.NewString(),
		404,
	)
	call(
		first,
		"POST",
		"/api/v1/devices/"+owner.DeviceID+"/overrides",
		map[string]any{"room_target_c": 22, "duration_sim_minutes": 30},
		uuid.NewString(),
		409,
	)
	// Seed one authenticated current state at this storage boundary; the browser test covers MQTT ingestion.
	sample := protocol.Sample{
		Sequence:      1,
		Elapsed:       60,
		Indoor:        21,
		Outdoor:       -3,
		Supply:        30,
		ObservedAt:    protocol.StartTime.Add(time.Minute),
		RecordedAt:    time.Now().UTC(),
		ConfigVersion: 1,
	}
	if _, err = db.Exec(
		t.Context(),
		`UPDATE devices SET enrollment='active',latest=$2,last_received_at=now(),accepted_version=1,reported_version=1 WHERE id=$1`,
		owner.DeviceID,
		sample,
	); err != nil {
		t.Fatal(err)
	}
	commandJSON := call(
		first,
		"POST",
		"/api/v1/devices/"+owner.DeviceID+"/overrides",
		map[string]any{"room_target_c": 22, "duration_sim_minutes": 30},
		uuid.NewString(),
		202,
	)
	var command api.Command
	if err = json.Unmarshal(commandJSON, &command); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(
		t.Context(),
		`SELECT count(*) FROM outbox WHERE command_id=$1`,
		command.Id,
	).Scan(
		&count,
	); err != nil ||
		count != 1 {
		t.Fatalf("command missing transactional outbox: %d %v", count, err)
	}
	if command.Stage != "persisted" {
		t.Fatal("API claimed execution before publication")
	}
	base := "/api/v1/devices/" + owner.DeviceID
	optimizationBody := map[string]any{"enabled": true, "scenario": "winter"}
	call(second, "PUT", base+"/optimization", optimizationBody, uuid.NewString(), 404)
	optimizationKey := uuid.NewString()
	call(first, "PUT", base+"/optimization", optimizationBody, optimizationKey, 202)
	call(first, "PUT", base+"/optimization", optimizationBody, optimizationKey, 202)
	var version int
	var target *float64
	if err = db.QueryRow(
		t.Context(),
		`SELECT (config->>'config_version')::int,override_target FROM devices WHERE id=$1`,
		owner.DeviceID,
	).Scan(
		&version,
		&target,
	); err != nil || version != 2 || target == nil ||
		*target != 22 {
		t.Fatal("optimization retry changed version or displaced manual intent", err)
	}
	comparisonBody := map[string]string{"scenario": "winter", "start": protocol.StartTime.Format(time.RFC3339)}
	comparisonKey := uuid.NewString()
	call(second, "POST", base+"/comparisons", comparisonBody, uuid.NewString(), 404)
	reportJSON := call(first, "POST", base+"/comparisons", comparisonBody, comparisonKey, 201)
	if repeated := call(
		first,
		"POST",
		base+"/comparisons",
		comparisonBody,
		comparisonKey,
		201,
	); !bytes.Equal(
		reportJSON,
		repeated,
	) {
		t.Fatal("comparison retry changed immutable outcome")
	}
	var report api.Comparison
	if err = json.Unmarshal(
		reportJSON,
		&report,
	); err != nil || !report.Plan.Adopted || report.Plan.Comparable ||
		report.Plan.Savings != nil {
		t.Fatal("comparison not evaluated", err)
	}
	artifact := base + "/comparisons/" + report.Id + "/artifact"
	call(second, "GET", artifact, nil, "", 404)
	call(second, "POST", artifact, nil, "", 404)
	call(first, "POST", artifact, nil, "", 503) // Export failure must leave the comparison in PostgreSQL.
	call(first, "GET", base+"/comparisons", nil, "", 200)
	// A confirmed slot must not block the next slot until its wall-clock delivery TTL.
	d, err := loadDevice(t.Context(), db, owner.DeviceID, false)
	if err != nil {
		t.Fatal(err)
	}
	var previous protocol.Command
	var previousData []byte
	if err = db.QueryRow(
		t.Context(),
		`SELECT payload FROM commands WHERE id=$1`,
		command.Id,
	).Scan(
		&previousData,
	); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(previousData, &previous); err != nil {
		t.Fatal(err)
	}
	previous.Generation = d.Config.Generation
	previous.ConfigVersion = d.Config.Version
	previous.ValidUntil = sample.ObservedAt
	previous.ExpiresAt = time.Now().Add(30 * time.Second)
	if _, err = db.Exec(
		t.Context(),
		`UPDATE commands SET payload=$2,stage='confirmed',stage_rank=5 WHERE id=$1`,
		previous.ID,
		previous,
	); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(
		t.Context(),
		`UPDATE devices SET override_target=NULL,override_until=NULL,accepted_version=2,reported_version=2,last_received_at=now() WHERE id=$1`,
		owner.DeviceID,
	); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = app.resolve(t.Context(), owner.DeviceID); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.QueryRow(
		t.Context(),
		`SELECT count(*) FROM commands WHERE device_id=$1`,
		owner.DeviceID,
	).Scan(
		&count,
	); err != nil ||
		count != 2 {
		t.Fatal("confirmed prior slot blocked successor or created duplicate commands", count, err)
	}
	if _, err = db.Exec(
		t.Context(),
		`UPDATE devices SET last_received_at=now()-interval '1 minute' WHERE id=$1`,
		owner.DeviceID,
	); err != nil {
		t.Fatal(err)
	}
	call(first, "PUT", base+"/optimization", optimizationBody, uuid.NewString(), 409)
	optimizationBody["enabled"] = false
	call(first, "PUT", base+"/optimization", optimizationBody, uuid.NewString(), 202)
	var optedOut bool
	if err = db.QueryRow(
		t.Context(),
		`SELECT NOT (config->'automation'->>'optimization_enabled')::bool FROM devices WHERE id=$1`,
		owner.DeviceID,
	).Scan(
		&optedOut,
	); err != nil ||
		!optedOut {
		t.Fatal("offline opt-out was not persisted", err)
	}

	// Operator role is explicit; neither an owner nor another tenant may dispatch.
	operatorAccess := local.OperatorAccess{
		OrganizationID: owner.OrganizationID,
		UserID:         uuid.NewString(),
		Login:          "operator@example.local",
		Password:       local.Secret(),
	}
	if err = app.SeedOperator(t.Context(), operatorAccess); err != nil {
		t.Fatal(err)
	}
	operatorClient := login(local.Seed{Login: operatorAccess.Login, Password: operatorAccess.Password})
	fleetPath := "/api/v1/organizations/" + owner.OrganizationID + "/fleet"
	eventsPath := "/api/v1/organizations/" + owner.OrganizationID + "/flexibility-events"
	call(first, "GET", fleetPath, nil, "", 404)
	call(second, "GET", fleetPath, nil, "", 404)
	call(operatorClient, "GET", fleetPath, nil, "", 200)
	call(first, "GET", "/api/v1/organizations/"+owner.OrganizationID+"/metrics", nil, "", 404)
	call(operatorClient, "GET", "/api/v1/organizations/"+owner.OrganizationID+"/metrics", nil, "", 200)
	d, err = loadDevice(t.Context(), db, owner.DeviceID, false)
	if err != nil {
		t.Fatal(err)
	}
	d.Config.Automation.Flexibility = true
	d.Config.Automation.Optimization = true
	if _, err = db.Exec(
		t.Context(),
		`UPDATE devices SET config=$2,accepted_version=$3,reported_version=$3,last_received_at=now(),override_target=NULL,override_until=NULL WHERE id=$1`,
		d.ID,
		d.Config,
		d.Config.Version,
	); err != nil {
		t.Fatal(err)
	}
	eventBody := map[string]int{"duration_sim_minutes": 15}
	eventKey := uuid.NewString()
	call(first, "POST", eventsPath, eventBody, eventKey, 404)
	eventData := call(operatorClient, "POST", eventsPath, eventBody, eventKey, 201)
	if repeated := call(
		operatorClient,
		"POST",
		eventsPath,
		eventBody,
		eventKey,
		201,
	); !bytes.Equal(
		repeated,
		eventData,
	) {
		t.Fatal("dispatch retry created a new intention")
	}
	var event api.FlexibilityEvent
	if err = json.Unmarshal(eventData, &event); err != nil {
		t.Fatal(err)
	}
	for _, participant := range event.Participants {
		if participant.CommandCounts == nil || len(participant.CommandCounts) != 0 {
			t.Fatalf("new dispatch returned invalid or prior command evidence: %+v", participant)
		}
	}
	call(operatorClient, "POST", eventsPath, eventBody, uuid.NewString(), 409)
	call(second, "GET", eventsPath+"/"+event.Id, nil, "", 404)
	if err = app.resolve(t.Context(), owner.DeviceID); err != nil {
		t.Fatal(err)
	}
	var flexCommand protocol.Command
	var flexData []byte
	if err = db.QueryRow(
		t.Context(),
		`SELECT payload FROM commands WHERE device_id=$1 ORDER BY (payload->>'command_seq')::bigint DESC LIMIT 1`,
		owner.DeviceID,
	).Scan(
		&flexData,
	); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(
		flexData,
		&flexCommand,
	); err != nil || flexCommand.Source != "flexibility_dispatch" ||
		flexCommand.Parameters.Room < d.Config.Comfort.Min {
		t.Fatal("dispatch bypassed resolver", flexCommand, err)
	}
	var evidence api.FlexibilityEvent
	if err = json.Unmarshal(call(operatorClient, "GET", eventsPath+"/"+event.Id, nil, "", 200), &evidence); err != nil {
		t.Fatal(err)
	}
	foundEvidence := false
	for _, participant := range evidence.Participants {
		if participant.DeviceId != owner.DeviceID {
			if len(participant.CommandCounts) != 0 {
				t.Fatalf("excluded device gained command evidence: %+v", participant)
			}
			continue
		}
		foundEvidence = true
		if len(participant.CommandCounts) != 1 || participant.CommandCounts[0].Stage != "persisted" ||
			participant.CommandCounts[0].Count != 1 {
			t.Fatalf("dispatch evidence must include only its new command, got %+v", participant)
		}
	}
	if !foundEvidence {
		t.Fatal("dispatch evidence omitted the eligible device")
	}
	call(
		first,
		"POST",
		base+"/overrides",
		map[string]any{"room_target_c": 21.5, "duration_sim_minutes": 15},
		uuid.NewString(),
		202,
	)
	var participantState string
	if err = db.QueryRow(
		t.Context(),
		`SELECT status FROM flexibility_participants WHERE event_id=$1 AND device_id=$2`,
		event.Id,
		owner.DeviceID,
	).Scan(
		&participantState,
	); err != nil ||
		participantState != "excluded" {
		t.Fatal("owner override did not withdraw dispatch", err)
	}
	if err = app.send(
		t.Context(),
		protocol.Topic(owner.OrganizationID, owner.DeviceID, "command"),
		flexData,
		false,
	); !errors.Is(
		err,
		errObsoleteDelivery,
	) {
		t.Fatal("old dispatch authority survived override", err)
	}
	call(operatorClient, "DELETE", eventsPath+"/"+event.Id, nil, "", 204)
	call(operatorClient, "GET", eventsPath+"/"+event.Id, nil, "", 200)

	// Reset is an acknowledged operation; HTTP and broker publication never imply equipment execution.
	runPath := base + "/run"
	resetBody := map[string]any{"action": "reset", "rate": 10, "model": "heating-dhw"}
	call(second, "POST", runPath, resetBody, uuid.NewString(), 404)
	resetKey := uuid.NewString()
	resetResponse := call(first, "POST", runPath, resetBody, resetKey, 202)
	if repeated := call(first, "POST", runPath, resetBody, resetKey, 202); !bytes.Equal(resetResponse, repeated) {
		t.Fatal("reset retry changed identity")
	}
	var run api.RunState
	if err = json.Unmarshal(
		resetResponse,
		&run,
	); err != nil || run.Status != "pending" || run.Epoch != 2 ||
		run.RunId == owner.RunID {
		t.Fatal("invalid reset response", err)
	}
	call(first, "POST", runPath, resetBody, uuid.NewString(), 409)
	var op protocol.RunOperation
	var data []byte
	if err = db.QueryRow(
		t.Context(),
		`SELECT payload FROM run_operations WHERE device_id=$1`,
		owner.DeviceID,
	).Scan(
		&data,
	); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &op); err != nil {
		t.Fatal(err)
	}
	acknowledgement := protocol.RunReport{
		SchemaVersion: 1,
		ID:            op.ID,
		DeviceID:      op.DeviceID,
		RunID:         op.RunID,
		Epoch:         op.Epoch,
		Sequence:      op.Sequence,
	}
	data, err = json.Marshal(acknowledgement)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.ingestRun(t.Context(), other.OrganizationID, owner.DeviceID, data); err == nil {
		t.Fatal("cross-tenant run acknowledgement accepted")
	}
	for range 2 {
		if err = app.ingestRun(t.Context(), owner.OrganizationID, owner.DeviceID, data); err != nil {
			t.Fatal(err)
		}
	}
	if err = json.Unmarshal(
		call(first, "GET", runPath, nil, "", 200),
		&run,
	); err != nil || run.Status != "ready" ||
		run.Rate != 10 {
		t.Fatal("run not acknowledged", err)
	}
	oldSample := protocol.Sample{
		Sequence:     2,
		Elapsed:      120,
		ObservedAt:   protocol.StartTime.Add(2 * time.Minute),
		RecordedAt:   time.Now().UTC(),
		Indoor:       21,
		Outdoor:      -3,
		Supply:       30,
		RoomTarget:   21,
		SupplyTarget: 40,
		EnergyWh:     30,
		Mode:         "baseline",
		Compressor:   "off",
		Alarms:       []string{},
		Quality:      []string{},
	}
	historical := protocol.Batch{
		SchemaVersion: 1,
		DeviceID:      owner.DeviceID,
		RunID:         owner.RunID,
		BootID:        uuid.NewString(),
		ID:            uuid.NewString(),
		Origin:        "simulation",
		SentAt:        time.Now().UTC(),
		Samples:       []protocol.Sample{oldSample},
	}
	data, err = json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := app.persistTelemetry(t.Context(), owner.OrganizationID, owner.DeviceID, data)
	if err != nil || receipt.Status != "committed" {
		t.Fatal("old run history lost", err, receipt.Code)
	}
	var latest []byte
	if err = db.QueryRow(
		t.Context(),
		`SELECT latest FROM devices WHERE id=$1`,
		owner.DeviceID,
	).Scan(
		&latest,
	); err != nil ||
		latest != nil {
		t.Fatal("historical telemetry refreshed current run", err)
	}
	replayBody := map[string]any{
		"run_id": owner.RunID,
		"from":   protocol.StartTime,
		"to":     protocol.StartTime.Add(3 * time.Minute),
	}
	call(second, "POST", base+"/replays", replayBody, uuid.NewString(), 404)
	call(first, "POST", base+"/replays", replayBody, uuid.NewString(), 422)
	historical.ID = uuid.NewString()
	historical.Samples[0].Sequence++
	historical.Samples[0].Elapsed = 180
	historical.Samples[0].ObservedAt = protocol.StartTime.Add(3 * time.Minute)
	historical.Samples[0].EnergyWh = 60
	data, err = json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.persistTelemetry(t.Context(), owner.OrganizationID, owner.DeviceID, data); err != nil {
		t.Fatal(err)
	}
	call(first, "POST", base+"/replays", replayBody, uuid.NewString(), 201)
	call(first, "GET", base+"/replays", nil, "", 200)
	// Hot-water settings require this model's capability and valid hysteresis.
	settingsData := call(first, "GET", base+"/settings", nil, "", 200)
	var settings api.DeviceSettings
	if err = json.Unmarshal(settingsData, &settings); err != nil || settings.Model != "heating-dhw" {
		t.Fatal("missing hot-water settings", err)
	}
	call(second, "GET", base+"/settings", nil, "", 404)
	settingsBody := map[string]any{
		"preset":             "comfort",
		"flexibility_opt_in": false,
		"hot_water":          map[string]any{"start_c": 50, "stop_c": 51},
	}
	settingsTag := fmt.Sprintf(`etag:"%d"`, settings.Version)
	call(first, "PUT", base+"/settings", settingsBody, settingsTag, 422)
	settingsBody["hot_water"] = map[string]any{"start_c": 50, "stop_c": 55}
	call(first, "PUT", base+"/settings", settingsBody, settingsTag, 202)
	call(first, "PUT", base+"/settings", settingsBody, settingsTag, 409)
	call(second, "GET", base+"/history?day=2026-01-15", nil, "", 404)
	call(first, "GET", base+"/history?day=2026-01-15&run_id="+owner.RunID, nil, "", 200)
	call(first, "GET", base+"/history?day=2026-01-15&run_id="+other.RunID, nil, "", 404)
	call(first, "GET", base+"/history?from=2026-01-15T00:00:00Z&to=2026-02-15T00:00:00Z", nil, "", 422)

	// Retention deletes bounded expired history but preserves immutable replay reports.
	if _, err = db.Exec(
		t.Context(),
		`UPDATE telemetry SET received_at=now()-interval '8 days' WHERE device_id=$1`,
		owner.DeviceID,
	); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = app.retain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var retained int
	if err = db.QueryRow(
		t.Context(),
		`SELECT count(*) FROM telemetry WHERE device_id=$1`,
		owner.DeviceID,
	).Scan(
		&retained,
	); err != nil ||
		retained != 0 {
		t.Fatal("old telemetry survived retention", retained, err)
	}
	call(first, "GET", base+"/replays", nil, "", 200)
	historical.ID = uuid.NewString()
	historical.Samples[0].RecordedAt = time.Now().Add(-8 * 24 * time.Hour)
	data, err = json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = app.persistTelemetry(t.Context(), owner.OrganizationID, owner.DeviceID, data)
	if err != nil || receipt.Code != "outside_retention" {
		t.Fatal("expired backfill resurrected evidence", receipt, err)
	}
	// Retiring a direct identity immediately fences delivery; broker revocation is retried by the worker.
	call(second, "DELETE", base+"/connection", nil, "", 404)
	call(first, "DELETE", base+"/connection", nil, "", 204)
	d, err = loadDevice(t.Context(), db, owner.DeviceID, false)
	if err != nil || d.Enrollment != "revoked" {
		t.Fatal("direct identity not retired", err)
	}
	if err = app.send(
		t.Context(),
		protocol.Topic(owner.OrganizationID, owner.DeviceID, "command"),
		flexData,
		false,
	); !errors.Is(
		err,
		errObsoleteDelivery,
	) {
		t.Fatal("retired identity received control", err)
	}
	call(first, "POST", runPath, resetBody, uuid.NewString(), 409)

	// Database compound foreign keys must reject a cross-organization device/building link.
	if _, err = db.Exec(
		t.Context(),
		`UPDATE devices SET building_id=$2 WHERE id=$1`,
		other.DeviceID,
		building.Id,
	); err == nil {
		t.Fatal("cross-tenant foreign key accepted")
	}
	if _, err = db.Exec(t.Context(), `DELETE FROM memberships WHERE user_id=$1`, other.UserID); err != nil {
		t.Fatal(err)
	}
	call(second, "GET", "/api/v1/organizations/"+other.OrganizationID+"/buildings", nil, "", 404)
	var stored []byte
	if err = db.QueryRow(
		t.Context(),
		`SELECT token_hash FROM sessions WHERE user_id=$1`,
		owner.UserID,
	).Scan(
		&stored,
	); err != nil ||
		len(stored) != len(identity.Digest("token")) {
		t.Fatal("session token not hashed")
	}
}
