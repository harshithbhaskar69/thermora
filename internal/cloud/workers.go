package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
)

func (a *App) Run(ctx context.Context) error {
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		retainAt := time.Now().Add(5 * time.Minute)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if time.Now().After(retainAt) {
					work, cancel := context.WithTimeout(ctx, 30*time.Second)
					err := a.retain(work)
					cancel()
					if err != nil {
						slog.Warn("retention deferred", "error", err)
					}
					retainAt = time.Now().Add(5 * time.Minute)
				}
				if err := a.revokeDevices(ctx); err != nil {
					slog.Warn("device revocation deferred", "error", err)
				}
				if err := a.revokeManufacturers(ctx); err != nil {
					slog.Warn("manufacturer revocation deferred", "error", err)
				}
				if err := a.pollManufacturers(ctx); err != nil {
					slog.Warn("manufacturer polling deferred", "error", err)
				}
			}
		}
	}()
	defer func() { <-pollDone }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	renewAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-a.MQTT.Incoming:
			work, cancel := context.WithTimeout(ctx, 8*time.Second)
			err := a.ingest(work, msg)
			cancel()
			if err != nil {
				slog.Warn("ingestion deferred or rejected", "topic", msg.Topic, "error", err)
			}
		case <-tick.C:
			if err := a.finishFlexibility(ctx); err != nil {
				slog.Warn("flexibility completion deferred", "error", err)
			}
			if err := a.dispatch(ctx); err != nil {
				slog.Warn("outbox delivery deferred", "error", err)
			}
			renew := !time.Now().Before(renewAt)
			if renew {
				renewAt = time.Now().Add(10 * time.Second)
			}
			if err := a.reconcile(ctx, renew); err != nil {
				slog.Warn("control reconciliation deferred", "error", err)
			}
		}
	}
}

func (a *App) dispatch(ctx context.Context) error {
	rows, err := a.DB.Query(
		ctx,
		`SELECT id,topic,payload,retained,command_id,expires_at,attempts FROM outbox WHERE next_attempt_at<=now() ORDER BY id LIMIT 25`,
	)
	if err != nil {
		return err
	}
	type item struct {
		id       int64
		topic    string
		data     []byte
		retain   bool
		command  *string
		expiry   *time.Time
		attempts int
	}
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.topic, &i.data, &i.retain, &i.command, &i.expiry, &i.attempts); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, i := range items {
		if i.expiry != nil && time.Now().After(*i.expiry) {
			tx, err := a.DB.Begin(ctx)
			if err != nil {
				return err
			}
			if i.command != nil {
				_, err = tx.Exec(
					ctx,
					`UPDATE commands SET stage='expired',stage_rank=6 WHERE id=$1 AND stage_rank<4`,
					*i.command,
				)
			}
			if err == nil {
				_, err = tx.Exec(ctx, `DELETE FROM outbox WHERE id=$1`, i.id)
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				storage.Rollback(ctx, tx)
			}
			if err != nil {
				return err
			}
			continue
		}
		if err = a.send(ctx, i.topic, i.data, i.retain); errors.Is(err, errObsoleteDelivery) {
			tx, beginErr := a.DB.Begin(ctx)
			if beginErr != nil {
				return beginErr
			}
			if i.command != nil {
				_, err = tx.Exec(
					ctx,
					`UPDATE commands SET stage='superseded',stage_rank=6 WHERE id=$1 AND stage_rank<4`,
					*i.command,
				)
			} else {
				err = nil
			}
			if err == nil {
				_, err = tx.Exec(ctx, `DELETE FROM outbox WHERE id=$1`, i.id)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
			storage.Rollback(ctx, tx)
			if err != nil {
				return err
			}
			continue
		} else if err != nil {
			_, updateErr := a.DB.Exec(
				ctx,
				`UPDATE outbox SET attempts=attempts+1,next_attempt_at=now()+$2::interval WHERE id=$1`,
				i.id,
				(time.Duration(1<<min(i.attempts, 4)) * time.Second).String(),
			)
			if updateErr != nil {
				return updateErr
			}
			slog.Warn("device delivery deferred", "topic", i.topic, "error", err)
			continue
		}
		if strings.HasSuffix(i.topic, "/demo/action") {
			_, err = a.DB.Exec(ctx, `UPDATE outbox SET next_attempt_at=now()+interval '2 seconds' WHERE id=$1`, i.id)
		} else if i.command == nil {
			_, err = a.DB.Exec(ctx, `DELETE FROM outbox WHERE id=$1`, i.id)
		} else {
			tx, beginErr := a.DB.Begin(ctx)
			if beginErr != nil {
				return beginErr
			}
			_, err = tx.Exec(
				ctx,
				`UPDATE commands SET stage='published',stage_rank=1 WHERE id=$1 AND stage_rank=0`,
				*i.command,
			)
			if err == nil {
				_, err = tx.Exec(
					ctx,
					`UPDATE outbox SET attempts=attempts+1,next_attempt_at=now()+$2::interval WHERE id=$1`,
					i.id,
					(time.Duration(1<<min(i.attempts, 4)) * time.Second).String(),
				)
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				storage.Rollback(ctx, tx)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) reconcile(ctx context.Context, renew bool) error {
	rows, err := a.DB.Query(ctx, `SELECT id FROM devices WHERE enrollment='active' ORDER BY id LIMIT 25`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.resolve(ctx, id); err != nil {
			return err
		}
		d, err := loadDevice(ctx, a.DB, id, false)
		if err != nil {
			return err
		}
		if (d.OverrideTarget == nil && !d.Config.Automation.Optimization) || !renew {
			continue
		}
		var data []byte
		err = a.DB.QueryRow(
			ctx,
			`SELECT payload FROM commands WHERE device_id=$1 AND payload->>'run_id'=$2 AND stage IN ('accepted','applied','confirmed') ORDER BY created_at DESC LIMIT 1`,
			id,
			d.Run,
		).Scan(
			&data,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var c protocol.Command
		if err = json.Unmarshal(data, &c); err != nil {
			return err
		}
		if err = a.lease(ctx, d, c); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) lease(ctx context.Context, d deviceRecord, c protocol.Command) error {
	if c.Parameters.Mode != "remote" || c.Generation != d.Config.Generation || d.Reported != c.ConfigVersion ||
		d.Received == nil ||
		time.Since(*d.Received) > 15*time.Second ||
		d.Latest == nil ||
		!d.Latest.ObservedAt.Before(c.ValidUntil) {
		return nil
	}
	now := time.Now().UTC()
	return a.sendJSON(
		ctx,
		protocol.Topic(d.Org, d.ID, "control/lease"),
		protocol.Lease{
			SchemaVersion: 1,
			DeviceID:      d.ID,
			RunID:         d.Run,
			Generation:    c.Generation,
			Sequence:      c.Sequence,
			IssuedAt:      now,
			ValidUntil:    now.Add(30 * time.Second),
		},
		false,
	)
}
