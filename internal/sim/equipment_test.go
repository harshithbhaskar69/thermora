package sim_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/simonvetter/modbus"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/sim"
)

func TestModbusAtomicWriteAndUnitIsolation(t *testing.T) {
	t.Parallel()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "tcp://" + l.Addr().String()
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "equipment.db")
	e, err := sim.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, address, 1) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	}()
	c, err := modbus.NewClient(&modbus.ClientConfiguration{URL: address, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = c.Open()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = c.SetUnitId(1); err != nil {
		t.Fatal(err)
	}
	f := frame(42)
	if err = c.WriteRegisters(0, f[:]); err != nil {
		t.Fatal(err)
	}
	if err = c.WriteRegister(9, 5001); err == nil {
		t.Fatal("partial FC06 write accepted")
	}
	got, err := c.ReadRegisters(0, protocol.FrameCount, modbus.HOLDING_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	for i := range f {
		if got[i] != f[i] {
			t.Fatal("holding readback changed after rejected write")
		}
	}
	r, err := c.ReadRegisters(0, protocol.InputCount, modbus.INPUT_REGISTER)
	if err != nil || protocol.Uint32(r[18:20]) != 1 {
		t.Fatalf("application not reported: %v", err)
	}
	if err = c.SetUnitId(2); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReadRegisters(0, 1, modbus.INPUT_REGISTER); err == nil {
		t.Fatal("unconfigured unit leaked state")
	}
}
