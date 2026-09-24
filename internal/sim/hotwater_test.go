package sim_test

import (
	"math"
	"testing"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/sim"
)

func TestHotWaterControlAndHeatBalance(t *testing.T) {
	t.Parallel()
	now := time.Now()
	low, high := sim.NewState(1), sim.NewState(1)
	for _, pair := range []struct {
		s           *sim.State
		start, stop float64
	}{{&low, 40, 45}, {&high, 50, 55}} {
		s := pair.s
		s.Model = "heating-dhw"
		f := frame(42)
		f[20], f[21], f[22] = protocol.Centi(pair.start), protocol.Centi(pair.stop), 1
		if err := s.Apply(f, now); err != nil {
			t.Fatal(err)
		}
		before := *s
		s.Step(now)
		stored := 6*(s.Indoor-before.Indoor) + .116*(s.Buffer-before.Buffer) + .232*(s.Tank-before.Tank)
		net := (s.ThermalW/1000 + .25 - .18*(before.Indoor-before.Outdoor)) * 10 / 3600
		if math.Abs(stored-net) > 1e-10 {
			t.Fatal("model created or lost unaccounted heat", stored, net)
		}
		for range 10800 {
			s.Step(now.Add(time.Minute))
		}
		sample, err := protocol.SampleFromRegisters(s.Registers(now.Add(time.Minute)), now)
		if err != nil || sample.HotWater == nil || sample.Buffer == nil {
			t.Fatal("invalid tank telemetry", err)
		}
		if sample.HotWater.EnergyWh == 0 || sample.HotWater.EnergyWh > sample.EnergyWh || sample.ThermalEnergyWh == 0 {
			t.Fatal("invalid energy accounting")
		}
		if sample.HotWater.Start != pair.start || sample.HotWater.Stop != pair.stop {
			t.Fatal("setpoints not applied")
		}
	}
	if high.Tank <= low.Tank || high.DHWEnergyWh <= low.DHWEnergyWh {
		t.Fatal(
			"hot-water control did not affect physical state",
			low.Tank,
			high.Tank,
			low.DHWEnergyWh,
			high.DHWEnergyWh,
		)
	}
	bad := frame(40)
	bad[20], bad[21], bad[22] = 5000, 5100, 1
	if err := bad.Validate(1, 0); err == nil {
		t.Fatal("unsafe narrow hysteresis accepted")
	}
	high.Tank, high.On, high.LastSwitch = 62, true, high.Elapsed
	high.Step(now)
	if high.On || high.Registers(now)[17]&(1<<4) == 0 {
		t.Fatal("overheating must stop immediately and remain visible in register readback")
	}
}
