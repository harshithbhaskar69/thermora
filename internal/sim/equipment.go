package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/simonvetter/modbus"
	bolt "go.etcd.io/bbolt"

	"thermora.local/thermora/internal/protocol"
)

// Equipment serializes ticks and Modbus transactions into coherent, durable snapshots.
type Equipment struct {
	mu    sync.Mutex
	db    *bolt.DB
	state State
}

func Open(path string, epoch uint32) (*Equipment, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open equipment checkpoint: %w", err)
	}
	initialEpoch := epoch
	if initialEpoch == 0 {
		initialEpoch = 1
	}
	e := &Equipment{db: db, state: NewState(initialEpoch)}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("state"))
		if err != nil {
			return err
		}
		if data := b.Get([]byte("unit1")); data != nil {
			return json.Unmarshal(data, &e.state)
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load equipment checkpoint: %w", err)
	}
	if epoch != 0 && e.state.Epoch != epoch {
		_ = db.Close()
		return nil, fmt.Errorf("equipment epoch mismatch: persisted %d, configured %d", e.state.Epoch, epoch)
	}
	// A process restart never restores remote authority from a wall-clock timestamp.
	e.state.LastRenewal = time.Time{}
	return e, nil
}

func (e *Equipment) Close() error { return e.db.Close() }

func (e *Equipment) save(next State) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err = e.db.Update(
		func(tx *bolt.Tx) error { return tx.Bucket([]byte("state")).Put([]byte("unit1"), data) },
	); err != nil {
		return fmt.Errorf("persist equipment: %w", err)
	}
	e.state = next
	return nil
}

func (e *Equipment) Run(ctx context.Context, address string, rate int) (err error) {
	if rate < 1 || rate > 60 || (rate != 1 && rate != 10 && rate != 60) {
		return fmt.Errorf("unsupported live simulation rate %d", rate)
	}
	server, err := modbus.NewServer(
		&modbus.ServerConfiguration{
			URL:        address,
			Timeout:    15 * time.Second,
			MaxClients: 4,
			Logger:     log.New(slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug).Writer(), "", 0),
		},
		e,
	)
	if err != nil {
		return err
	}
	if err = server.Start(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, server.Stop()) }()
	ticker := time.NewTicker(10 * time.Second / time.Duration(rate))
	e.mu.Lock()
	if e.state.RunFrame[15] == 0 {
		e.state.Rate = uint16(rate)
	}
	e.mu.Unlock()
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			e.mu.Lock()
			next := e.state
			if !next.Paused {
				next.Step(now)
			}
			ticker.Reset(10 * time.Second / time.Duration(next.Rate))
			err = e.save(next)
			e.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
}

func (e *Equipment) HandleCoils(*modbus.CoilsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (e *Equipment) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (e *Equipment) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if req.UnitId != 1 || req.Quantity == 0 || uint32(req.Addr)+uint32(req.Quantity) > protocol.InputCount {
		return nil, modbus.ErrIllegalDataAddress
	}
	r := e.state.Registers(time.Now())
	return r[req.Addr : uint32(req.Addr)+uint32(req.Quantity)], nil
}

func (e *Equipment) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if req.UnitId == 1 && req.Addr == protocol.RunRegisterAddress && req.Quantity == protocol.RunRegisterCount {
		if !req.IsWrite {
			return append([]uint16(nil), e.state.RunFrame[:]...), nil
		}
		if len(req.Args) != protocol.RunRegisterCount {
			return nil, modbus.ErrIllegalDataAddress
		}
		var frame protocol.RunFrame
		copy(frame[:], req.Args)
		next := e.state
		if err := next.ApplyRun(frame); err != nil {
			return nil, modbus.ErrIllegalDataValue
		}
		if err := e.save(next); err != nil {
			return nil, modbus.ErrServerDeviceFailure
		}
		return nil, nil
	}
	if req.UnitId != 1 || req.Quantity == 0 || uint32(req.Addr)+uint32(req.Quantity) > protocol.FrameCount {
		return nil, modbus.ErrIllegalDataAddress
	}
	if !req.IsWrite {
		return append([]uint16(nil), e.state.Frame[req.Addr:uint32(req.Addr)+uint32(req.Quantity)]...), nil
	}
	if req.Addr != 0 || req.Quantity != protocol.FrameCount || len(req.Args) != protocol.FrameCount {
		return nil, modbus.ErrIllegalDataAddress
	}
	var f protocol.Frame
	copy(f[:], req.Args)
	next := e.state
	if err := next.Apply(f, time.Now()); err != nil {
		return nil, modbus.ErrIllegalDataValue
	}
	if err := e.save(next); err != nil {
		slog.Error("equipment checkpoint failed", "error", err)
		return nil, modbus.ErrServerDeviceFailure
	}
	return nil, nil
}

// ApplyRun uses a durable monotonic operation sequence; identical retries are harmless.
func (s *State) ApplyRun(f protocol.RunFrame) error {
	seq, previous := protocol.Uint32(f[8:10]), protocol.Uint32(s.RunFrame[8:10])
	if seq > 0 && seq == previous && f == s.RunFrame {
		return nil
	}
	if f[15] != 1 || seq <= previous || (f[13] != 1 && f[13] != 10 && f[13] != 60) || f[14] > 1 || f[12] < 1 ||
		f[12] > 4 {
		return errors.New("invalid or replayed run frame")
	}
	epoch := protocol.Uint32(f[10:12])
	if f[12] == 4 {
		if epoch != s.Epoch+1 || s.Epoch == ^uint32(0) {
			return errors.New("invalid reset epoch")
		}
		*s = NewState(epoch)
		if f[14] == 1 {
			s.Model = "heating-dhw"
		}
	} else if epoch != s.Epoch || (f[14] == 1) != (s.Model == "heating-dhw") {
		return errors.New("run epoch or model mismatch")
	}
	s.LastRenewal = time.Time{}
	s.RunFrame = f
	s.Rate = f[13]
	if f[12] == 1 {
		s.Paused = true
	}
	if f[12] == 2 {
		s.Paused = false
	}
	return nil
}
