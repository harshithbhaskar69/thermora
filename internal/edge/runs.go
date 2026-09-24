package edge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/simonvetter/modbus"
	bolt "go.etcd.io/bbolt"

	"thermora.local/thermora/internal/protocol"
)

func (a *agent) runOperation(ctx context.Context, data []byte) error {
	var op protocol.RunOperation
	if err := protocol.Decode(data, &op); err != nil {
		return err
	}
	if err := op.Validate(a.cfg.DeviceID, time.Now()); err != nil {
		return err
	}
	var previous protocol.RunOperation
	found, err := a.state.read("meta", "run-operation", &previous)
	if err != nil {
		return err
	}
	if found && op.Sequence < previous.Sequence {
		return errors.New("stale run operation")
	}
	if found && op.Sequence == previous.Sequence {
		aFrame, err := op.Frame()
		if err != nil {
			return err
		}
		bFrame, err := previous.Frame()
		if err != nil {
			return err
		}
		if aFrame != bFrame || op.RunID != previous.RunID {
			return errors.New("conflicting run operation")
		}
	} else {
		if op.Action == "reset" {
			if op.Epoch != a.cfg.RunEpoch+1 {
				return errors.New("invalid reset epoch")
			}
		} else if op.Epoch != a.cfg.RunEpoch || op.RunID != a.cfg.RunID {
			return errors.New("run identity mismatch")
		}
	}
	var current protocol.RunOperation
	if complete, e := a.state.read("meta", "run-current", &current); e != nil {
		return e
	} else if complete && current.ID == op.ID && current.Sequence == op.Sequence {
		return a.reportRun(ctx, op)
	}
	// Persist before equipment I/O. Re-delivery reconciles exact frame readback after interruption.
	if err = a.state.put("meta", "run-operation", op); err != nil {
		return err
	}
	a.active = nil
	a.lease = protocol.Lease{}
	a.leaseDeadline = time.Time{}
	frame, err := op.Frame()
	if err != nil {
		return err
	}
	if !a.connected {
		if err = a.modbus.Open(); err != nil {
			return err
		}
		a.connected = true
	}
	if err = a.modbus.WriteRegisters(protocol.RunRegisterAddress, frame[:]); err != nil {
		return err
	}
	readback, err := a.modbus.ReadRegisters(
		protocol.RunRegisterAddress,
		protocol.RunRegisterCount,
		modbus.HOLDING_REGISTER,
	)
	if err != nil {
		return err
	}
	if !equalRegisters(frame[:], readback) {
		return errors.New("ambiguous run readback")
	}
	// Run identity and configuration invalidation commit together; equipment uses the same epoch.
	err = a.state.db.Update(func(tx *bolt.Tx) error {
		encoded, err := json.Marshal(op)
		if err != nil {
			return err
		}
		meta := tx.Bucket([]byte("meta"))
		if err = meta.Put([]byte("run-current"), encoded); err != nil {
			return err
		}
		return meta.Delete([]byte("config"))
	})
	if err != nil {
		return err
	}
	if a.cfg.RunID != op.RunID {
		a.boot = uuid.NewString()
		a.sampleCounter = 0
	}
	a.cfg.RunID, a.cfg.RunEpoch = op.RunID, op.Epoch
	a.config = protocol.Configuration{}
	a.haveSample = false
	a.sampled = false
	a.lastHello = time.Time{}
	return a.reportRun(ctx, op)
}

func (a *agent) reportRun(ctx context.Context, op protocol.RunOperation) error {
	return a.mqtt.JSON(
		ctx,
		protocol.Topic(a.identity.Organization, a.cfg.DeviceID, "demo/event"),
		protocol.RunReport{
			SchemaVersion: 1,
			ID:            op.ID,
			DeviceID:      op.DeviceID,
			RunID:         op.RunID,
			Epoch:         op.Epoch,
			Sequence:      op.Sequence,
		},
		false,
	)
}
