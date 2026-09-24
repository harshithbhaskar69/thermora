package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
)

var errObsoleteDelivery = errors.New("control authority superseded")

type manufacturerRoute struct{ Device, Org, Connection, User string }

func (a *App) manufacturerRoute(ctx context.Context, device string) (manufacturerRoute, error) {
	var route manufacturerRoute
	err := a.DB.QueryRow(
		ctx,
		`SELECT e.device_id,e.organization_id,e.connection_id,c.user_id FROM cloud_enrollments e JOIN manufacturer_connections c ON c.id=e.connection_id WHERE e.device_id=$1`,
		device,
	).Scan(
		&route.Device,
		&route.Org,
		&route.Connection,
		&route.User,
	)
	return route, err
}

func (a *App) send(ctx context.Context, topic string, payload []byte, retained bool) error {
	a.controlMu.RLock()
	defer a.controlMu.RUnlock()

	parts := strings.Split(topic, "/")
	if len(parts) < 7 {
		return errors.New("invalid device destination")
	}
	d, err := loadDevice(ctx, a.DB, parts[5], false)
	if err != nil {
		return err
	}
	allowed, err := connected(ctx, a.DB, d.ID)
	if err != nil {
		return err
	}
	if d.Org != parts[3] || d.Enrollment != "active" || !allowed {
		if strings.HasSuffix(topic, "/demo/action") {
			return errors.New("run operation awaits reconnection")
		}
		return errObsoleteDelivery
	}
	suffix := strings.Join(parts[6:], "/")
	if suffix == "command" || suffix == "config/desired" || suffix == "control/lease" {
		var pending bool
		if err = a.DB.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM run_operations WHERE device_id=$1 AND completed_at IS NULL)`,
			d.ID,
		).Scan(
			&pending,
		); err != nil {
			return err
		}
		if pending {
			return errors.New("control waits for run acknowledgement")
		}
	}
	switch suffix {
	case "demo/action":
		var op protocol.RunOperation
		if err = protocol.Decode(payload, &op); err != nil {
			return err
		}
		if op.RunID != d.Run || op.Epoch != d.Epoch {
			return errObsoleteDelivery
		}
		op.Expires = time.Now().UTC().Add(30 * time.Second)
		payload, err = json.Marshal(op)
		if err != nil {
			return err
		}

	case "command":
		var command protocol.Command
		if err = protocol.Decode(payload, &command); err != nil {
			return err
		}
		if command.Validate(d.Config, d.Epoch, d.Config.Version, 0, time.Now()) != nil {
			return errObsoleteDelivery
		}
	case "config/desired":
		var config protocol.Configuration
		if err = protocol.Decode(payload, &config); err != nil {
			return err
		}
		if config.RunID != d.Run || config.Version != d.Config.Version || config.Generation != d.Config.Generation {
			return errObsoleteDelivery
		}
	case "control/lease":
		var lease protocol.Lease
		if err = protocol.Decode(payload, &lease); err != nil {
			return err
		}
		if lease.RunID != d.Run || lease.Generation != d.Config.Generation || !time.Now().Before(lease.ValidUntil) {
			return errObsoleteDelivery
		}
	}
	route, err := a.manufacturerRoute(ctx, parts[5])
	if errors.Is(err, pgx.ErrNoRows) {
		return a.MQTT.Publish(ctx, topic, payload, retained)
	}
	if err != nil {
		return err
	}
	if route.Org != parts[3] {
		return errors.New("manufacturer destination scope mismatch")
	}
	auth, err := a.connectionCredentials(ctx, route.Connection, route.User)
	if err != nil {
		return err
	}
	err = a.Manufacturer.Send(ctx, auth.token, route.Device, strings.Join(parts[6:], "/"), payload)
	return a.manufacturerError(ctx, auth, err)
}

func (a *App) sendJSON(ctx context.Context, topic string, value any, retained bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return a.send(ctx, topic, data, retained)
}

func (a *App) pollManufacturers(ctx context.Context) error {
	if a.Manufacturer == nil {
		return nil
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT e.device_id,e.organization_id,e.connection_id,c.user_id FROM cloud_enrollments e JOIN manufacturer_connections c ON c.id=e.connection_id JOIN devices d ON d.id=e.device_id WHERE d.enrollment='active' AND e.control_connected AND c.status='connected' ORDER BY e.device_id LIMIT 25`,
	)
	if err != nil {
		return err
	}
	routes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[manufacturerRoute])
	if err != nil {
		return err
	}
	// One bounded request per device (query capped at 25); a slow OEM route cannot
	// delay polling every healthy device. All work joins before the next poll cycle.
	var workers sync.WaitGroup
	for _, route := range routes {
		workers.Go(func() {
			work, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if err := a.pollManufacturer(work, route); err != nil {
				slog.Warn("manufacturer poll deferred", "device_id", route.Device, "error", err)
				if _, countErr := a.DB.Exec(
					work,
					`UPDATE devices SET manufacturer_failures=manufacturer_failures+1 WHERE id=$1`,
					route.Device,
				); countErr != nil {
					slog.Warn("manufacturer failure counter unavailable", "error", countErr)
				}
			}
		})
	}
	workers.Wait()
	return nil
}

func (a *App) pollManufacturer(ctx context.Context, route manufacturerRoute) error {
	auth, err := a.connectionCredentials(ctx, route.Connection, route.User)
	if err != nil {
		return err
	}
	state, err := a.Manufacturer.State(ctx, auth.token, route.Device)
	if err = a.manufacturerError(ctx, auth, err); err != nil {
		return err
	}
	for _, message := range state.Messages {
		switch message.Kind {
		case "telemetry", "command/event", "config/reported", "demo/event", "hello":
		default:
			return errors.New("unexpected manufacturer evidence kind")
		}
		if len(message.ID) != 64 || len(message.Payload) > protocol.MaxMessageBytes {
			return errors.New("invalid manufacturer evidence envelope")
		}
		err = a.ingestDevice(
			ctx,
			messaging.Message{Topic: protocol.Topic(route.Org, route.Device, message.Kind), Payload: message.Payload},
		)
		if err != nil {
			return err
		}
		if err = a.Manufacturer.Receipt(ctx, auth.token, route.Device, message.ID); err != nil {
			return a.manufacturerError(ctx, auth, err)
		}
	}
	return nil
}

// revokeManufacturers retries provider revocation while local authority remains withdrawn.
func (a *App) revokeManufacturers(ctx context.Context) error {
	if a.Manufacturer == nil {
		return nil
	}
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	rows, err := a.DB.Query(
		ctx,
		`SELECT id,tokens FROM manufacturer_connections WHERE revocation_pending ORDER BY id LIMIT 25`,
	)
	if err != nil {
		return err
	}
	type pending struct {
		ID     string
		Tokens []byte
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
	if err != nil {
		return err
	}
	for _, item := range items {
		data, err := a.Manufacturer.Decrypt(item.Tokens, "tokens:"+item.ID)
		if err != nil {
			return err
		}
		var tokens manufacturer.Tokens
		if err = json.Unmarshal(data, &tokens); err != nil {
			return err
		}
		for _, token := range []string{tokens.RefreshToken, tokens.AccessToken} {
			if err = a.Manufacturer.Revoke(ctx, token); err != nil {
				return err
			}
		}
		if _, err = a.DB.Exec(
			ctx,
			`UPDATE manufacturer_connections SET tokens=''::bytea,revocation_pending=false WHERE id=$1 AND status='disconnected'`,
			item.ID,
		); err != nil {
			return err
		}
	}
	return nil
}
