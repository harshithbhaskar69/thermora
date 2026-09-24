package sim_test

import (
	"testing"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/scenario"
	"thermora.local/thermora/internal/sim"
)

func frame(supply float64) protocol.Frame {
	cfg := protocol.NewConfiguration("device", "run", protocol.Comfort{Target: 21, Min: 20, Max: 22})
	c := protocol.Command{
		RunEpoch:      1,
		Sequence:      1,
		Generation:    1,
		ConfigVersion: 1,
		Parameters:    protocol.Parameters{Mode: "remote", Room: 22, Supply: supply},
		ValidFrom:     protocol.StartTime,
		ValidUntil:    protocol.StartTime.Add(2 * time.Hour),
	}
	return c.Frame(cfg)
}

func TestWeatherProfileSurvivesRemoteWatchdog(t *testing.T) {
	s := sim.NewState(1)
	now := time.Now()
	f := frame(40)
	f[15] = 1
	if err := s.Apply(f, now); err != nil {
		t.Fatal(err)
	}
	s.Step(now.Add(12 * time.Second))
	_, _, outdoor := scenario.Inputs(1, protocol.StartTime)
	if s.Remote || s.Outdoor != outdoor {
		t.Fatal("remote expiry erased physical scenario or retained remote control")
	}
	f[15] = 5
	if err := f.Validate(s.Epoch, s.Elapsed); err == nil {
		t.Fatal("unknown register weather profile accepted")
	}
}

func TestSupplyControlChangesEnergyAndTemperature(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	low, high := sim.NewState(1), sim.NewState(1)
	for _, pair := range []struct {
		s      *sim.State
		supply float64
	}{{&low, 30}, {&high, 48}} {
		if err := pair.s.Apply(frame(pair.supply), now); err != nil {
			t.Fatal(err)
		}
		for range 360 {
			pair.s.Step(now)
		}
	}
	if high.Indoor <= low.Indoor+.2 || high.EnergyWh <= low.EnergyWh {
		t.Fatalf("supply control had no modeled effect: low=%+v high=%+v", low, high)
	}
	if high.Elapsed != 3600 || low.Elapsed != 3600 {
		t.Fatal("model step drift")
	}
}

func TestCommandFencingAndWatchdog(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := sim.NewState(1)
	f := frame(40)
	if err := s.Apply(f, now); err != nil {
		t.Fatal(err)
	}
	before := s
	bad := f
	bad[9] = protocol.Centi(51)
	if err := s.Apply(bad, now); err == nil || s != before {
		t.Fatal("invalid write changed state")
	}
	bad = f
	bad[9] = protocol.Centi(41)
	if err := s.Apply(bad, now); err == nil {
		t.Fatal("same sequence accepted different content")
	}
	if err := s.Apply(f, now.Add(time.Second)); err != nil {
		t.Fatal("idempotent renewal failed", err)
	}
	if got := s.Registers(now.Add(12 * time.Second)); got[26] != 0 || got[17]&(1<<3) == 0 {
		t.Fatal("watchdog did not select baseline")
	}
}

func TestSignedRegistersAndAntiCycling(t *testing.T) {
	t.Parallel()
	s := sim.NewState(1)
	now := time.Now()
	s.Indoor = 19
	if registers := s.Registers(now); registers[16] != 2 || registers[17]&(1<<2) == 0 {
		t.Fatal("telemetry did not report the active minimum-off hold")
	}
	for range 30 {
		s.Step(now)
		if s.On {
			t.Fatal("started before minimum off time")
		}
	}
	s.Step(now)
	if !s.On {
		t.Fatal("did not start after dwell")
	}
	r := s.Registers(now)
	sample, err := protocol.SampleFromRegisters(r, now)
	if err != nil || sample.Outdoor != -3 || sample.Elapsed != 310 {
		t.Fatalf("bad signed/scaled snapshot: %+v %v", sample, err)
	}
}
