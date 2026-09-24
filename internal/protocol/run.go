package protocol

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

const RunRegisterAddress, RunRegisterCount = 100, 16

type RunOperation struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"operation_id"`
	DeviceID      string    `json:"device_id"`
	RunID         string    `json:"run_id"`
	Epoch         uint32    `json:"run_epoch"`
	Sequence      uint32    `json:"operation_seq"`
	Action        string    `json:"action"`
	Rate          uint16    `json:"rate"`
	Model         string    `json:"model"`
	Expires       time.Time `json:"expires_at_wall"`
}

type RunReport struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"operation_id"`
	DeviceID      string `json:"device_id"`
	RunID         string `json:"run_id"`
	Epoch         uint32 `json:"run_epoch"`
	Sequence      uint32 `json:"operation_seq"`
}

func (o RunOperation) Validate(device string, now time.Time) error {
	for _, id := range []string{o.ID, o.DeviceID, o.RunID} {
		if _, err := uuid.Parse(id); err != nil {
			return err
		}
	}
	identityMatches := o.SchemaVersion == 1 && o.DeviceID == device
	hasOrdering := o.Epoch != 0 && o.Sequence != 0
	validExpiry := now.Before(o.Expires) && !o.Expires.After(now.Add(35*time.Second))
	supportedRate := o.Rate == 1 || o.Rate == 10 || o.Rate == 60
	supportedModel := o.Model == "heating" || o.Model == "heating-dhw"
	if !identityMatches || !hasOrdering || !validExpiry || !supportedRate || !supportedModel {
		return errors.New("invalid run operation")
	}
	switch o.Action {
	case "pause", "resume", "rate", "reset":
		return nil
	default:
		return errors.New("invalid run action")
	}
}

// RunFrame is simulator-only administration on the existing private Modbus network.
// It is not part of a real manufacturer's equipment register map.
type RunFrame [RunRegisterCount]uint16

func (o RunOperation) Frame() (RunFrame, error) {
	var f RunFrame
	id, err := uuid.Parse(o.ID)
	if err != nil {
		return f, err
	}
	for i := 0; i < 8; i++ {
		f[i] = binary.BigEndian.Uint16(id[2*i : 2*i+2])
	}
	Put32(f[8:10], o.Sequence)
	Put32(f[10:12], o.Epoch)
	switch o.Action {
	case "pause":
		f[12] = 1
	case "resume":
		f[12] = 2
	case "rate":
		f[12] = 3
	case "reset":
		f[12] = 4
	default:
		return f, errors.New("invalid run action")
	}
	f[13] = o.Rate
	f[15] = 1
	if o.Model == "heating-dhw" {
		f[14] = 1
	}
	return f, nil
}
