package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func runState(ctx context.Context, q queryer, d deviceRecord) (api.RunState, error) {
	view := api.RunState{RunId: d.Run, Epoch: int(d.Epoch), Rate: 60, Model: "heating", Status: "ready"}
	err := q.QueryRow(
		ctx,
		`SELECT rate,paused,model FROM simulation_runs WHERE id=$1 AND device_id=$2`,
		d.Run,
		d.ID,
	).Scan(
		&view.Rate,
		&view.Paused,
		&view.Model,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return view, err
	}
	var data []byte
	var completed *time.Time
	err = q.QueryRow(
		ctx,
		`SELECT payload,completed_at FROM run_operations WHERE device_id=$1 ORDER BY sequence DESC LIMIT 1`,
		d.ID,
	).Scan(
		&data,
		&completed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	var op protocol.RunOperation
	if err = json.Unmarshal(data, &op); err != nil {
		return view, err
	}
	view.OperationId = &op.ID
	view.Action = &op.Action
	if completed == nil {
		view.Status = "pending"
	}
	return view, nil
}

func (a *App) GetRun(ctx context.Context, r api.GetRunRequestObject) (api.GetRunResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	d, err := loadDevice(ctx, a.DB, r.DeviceId, false)
	if err != nil {
		return nil, err
	}
	v, err := runState(ctx, a.DB, d)
	return api.GetRun200JSONResponse(v), err
}

func (a *App) ChangeRun(ctx context.Context, r api.ChangeRunRequestObject) (api.ChangeRunResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if r.Body.Rate < 1 || r.Body.Rate > 60 || (r.Body.Rate != 1 && r.Body.Rate != 10 && r.Body.Rate != 60) {
		return nil, fail(422, "invalid_rate", "Choose 1, 10 or 60 times simulated time.")
	}
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	v, err := idempotent(
		ctx,
		a.DB,
		s.userID,
		"run:"+r.DeviceId,
		r.Params.IdempotencyKey,
		r.Body,
		func(tx pgx.Tx) (api.RunState, error) {
			d, err := loadDevice(ctx, tx, r.DeviceId, true)
			if err != nil {
				return api.RunState{}, err
			}
			allowed, err := connected(ctx, tx, d.ID)
			if err != nil {
				return api.RunState{}, err
			}
			if !allowed || d.Enrollment != "active" {
				return api.RunState{}, fail(
					409,
					"device_disconnected",
					"Connect and activate equipment before changing its run.",
				)
			}
			state, err := runState(ctx, tx, d)
			if err != nil {
				return state, err
			}
			if state.Status == "pending" {
				return state, fail(409, "run_operation_pending", "Wait for the current operation to be acknowledged.")
			}
			if r.Body.Action != "reset" && string(r.Body.Model) != string(state.Model) {
				return state, fail(422, "reset_required", "Changing the thermal model requires an explicit reset.")
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO simulation_runs(id,device_id,epoch) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`,
				d.Run,
				d.ID,
				d.Epoch,
			); err != nil {
				return state, err
			}
			if r.Body.Action == "reset" {
				if d.Epoch == ^uint32(0) {
					return state, fail(409, "epoch_exhausted", "Provision a new simulation identity.")
				}
				if _, err = tx.Exec(
					ctx,
					`UPDATE simulation_runs SET finished_at=now() WHERE id=$1`,
					d.Run,
				); err != nil {
					return state, err
				}
				d.Run = uuid.NewString()
				d.Epoch++
				d.Config.RunID = d.Run
				d.Config.HotWater = protocol.HotWater{}
				d.Config.ModelVersion = "zone-supply-v1"
				if r.Body.Model == "heating-dhw" {
					d.Config.ModelVersion = "zone-buffer-dhw-v1"
					d.Config.HotWater = protocol.HotWater{Enabled: true, Start: 45, Stop: 50}
				}
				d.Latest = nil
				d.Sequence = 0
				if _, err = tx.Exec(
					ctx,
					`INSERT INTO simulation_runs(id,device_id,epoch,rate,model) VALUES($1,$2,$3,$4,$5)`,
					d.Run,
					d.ID,
					d.Epoch,
					r.Body.Rate,
					r.Body.Model,
				); err != nil {
					return state, err
				}
			}
			d.Config.Version++
			d.Config.Generation++
			d.Config.IssuedAt = time.Now().UTC()
			if r.Body.Action == "reset" {
				d.Config.Automation.Optimization = false
			}
			if err = excludeFlexibility(ctx, tx, d.ID, "run_changed"); err != nil {
				return state, err
			}
			if err = fenceDevice(ctx, tx, d); err != nil {
				return state, err
			}
			if _, err = tx.Exec(
				ctx,
				`UPDATE devices SET run_id=$2,run_epoch=$3,config=$4,command_seq=$5,last_received_at=NULL,override_target=NULL,override_until=NULL,accepted_version=0,reported_version=0,latest=NULL,latest_elapsed=-1 WHERE id=$1`,
				d.ID,
				d.Run,
				d.Epoch,
				d.Config,
				d.Sequence,
			); err != nil {
				return state, err
			}
			var seq uint32
			if err = tx.QueryRow(
				ctx,
				`SELECT COALESCE(max(sequence),0)+1 FROM run_operations WHERE device_id=$1`,
				d.ID,
			).Scan(
				&seq,
			); err != nil {
				return state, err
			}
			if r.Body.Rate < 1 || r.Body.Rate > 60 {
				return state, fail(422, "invalid_rate", "Unsupported rate.")
			}
			op := protocol.RunOperation{
				SchemaVersion: 1,
				ID:            uuid.NewString(),
				DeviceID:      d.ID,
				RunID:         d.Run,
				Epoch:         d.Epoch,
				Sequence:      seq,
				Action:        string(r.Body.Action),
				Rate:          uint16(r.Body.Rate),
				Model:         string(r.Body.Model),
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO run_operations(id,device_id,run_id,sequence,payload) VALUES($1,$2,$3,$4,$5)`,
				op.ID,
				d.ID,
				d.Run,
				seq,
				op,
			); err != nil {
				return state, err
			}
			if _, err = tx.Exec(
				ctx,
				`INSERT INTO outbox(organization_id,device_id,topic,payload) VALUES($1,$2,$3,$4)`,
				d.Org,
				d.ID,
				protocol.Topic(d.Org, d.ID, "demo/action"),
				op,
			); err != nil {
				return state, err
			}
			if err = audit(ctx, tx, d.Org, s.userID, "run_"+op.Action, d.ID, s.id); err != nil {
				return state, err
			}
			return runState(ctx, tx, d)
		},
	)
	return api.ChangeRun202JSONResponse(v), err
}

func (a *App) ingestRun(ctx context.Context, org, device string, data []byte) error {
	var report protocol.RunReport
	if err := protocol.Decode(data, &report); err != nil {
		return err
	}
	if report.SchemaVersion != 1 || report.DeviceID != device {
		return errors.New("invalid run report")
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, device, true)
	if err != nil {
		return err
	}
	if d.Org != org || d.Run != report.RunID || d.Epoch != report.Epoch {
		return errors.New("stale run report")
	}
	var raw []byte
	if err = tx.QueryRow(
		ctx,
		`SELECT payload FROM run_operations WHERE id=$1 AND device_id=$2 AND sequence=$3`,
		report.ID,
		device,
		report.Sequence,
	).Scan(
		&raw,
	); err != nil {
		return err
	}
	var op protocol.RunOperation
	if err = json.Unmarshal(raw, &op); err != nil {
		return err
	}
	if op.RunID != report.RunID || op.Epoch != report.Epoch {
		return errors.New("conflicting run acknowledgement")
	}
	tag, err := tx.Exec(
		ctx,
		`UPDATE run_operations SET completed_at=now() WHERE id=$1 AND completed_at IS NULL`,
		report.ID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if _, err = tx.Exec(
			ctx,
			`UPDATE simulation_runs SET rate=$2,model=$3,paused=CASE WHEN $4='pause' THEN true WHEN $4 IN ('resume','reset') THEN false ELSE paused END WHERE id=$1`,
			d.Run,
			op.Rate,
			op.Model,
			op.Action,
		); err != nil {
			return err
		}
		if _, err = tx.Exec(
			ctx,
			`DELETE FROM outbox WHERE device_id=$1 AND topic=$2 AND payload->>'operation_id'=$3`,
			device,
			protocol.Topic(org, device, "demo/action"),
			op.ID,
		); err != nil {
			return err
		}
		if err = queueConfig(ctx, tx, d); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
