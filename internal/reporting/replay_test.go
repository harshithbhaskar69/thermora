package reporting

import (
	"math"
	"testing"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/scenario"
	"thermora.local/thermora/internal/sim"
)

func TestObservedReplayRequiresEvidenceAndPreservesManualPolicy(t *testing.T) {
	t.Parallel()
	for _, water := range []bool{false, true} {
		cfg := protocol.NewConfiguration("device", "run", protocol.Comfort{Min: 20, Target: 21, Max: 22})
		cfg.WeatherProfile = 1
		state := sim.NewState(1)
		if water {
			state.Model = "heating-dhw"
			cfg.HotWater = protocol.HotWater{Enabled: true, Start: 45, Stop: 50}
		}
		wall := time.Unix(100, 0)
		var readings []Reading
		for i := 0; i <= 1440; i++ {
			at := protocol.StartTime.Add(time.Duration(state.Elapsed) * time.Second)
			_, _, state.Outdoor = scenario.Inputs(1, at)
			command := protocol.Command{
				RunEpoch:      1,
				Sequence:      1,
				Generation:    cfg.Generation,
				ConfigVersion: cfg.Version,
				ValidFrom:     protocol.StartTime,
				ValidUntil:    protocol.StartTime.Add(5 * time.Hour),
				Parameters: protocol.Parameters{
					Mode:   "remote",
					Room:   21.5,
					Supply: sim.BaselineSupply(state.Indoor, state.Outdoor, 21.5),
				},
			}
			state.Frame = command.Frame(cfg)
			state.LastRenewal = wall
			if i%6 == 0 {
				sample, err := protocol.SampleFromRegisters(state.Registers(wall), wall)
				if err != nil {
					t.Fatal(err)
				}
				sample.ConfigVersion = cfg.Version
				readings = append(
					readings,
					Reading{Sample: sample, Configuration: &cfg, Tariff: "variable", Source: "manual_override"},
				)
			}
			state.Step(wall)
		}
		result, err := ReplayObserved(readings)
		if err != nil || !result.Ready || result.ModelEnergyError == nil || *result.ModelEnergyError > .02 {
			t.Fatalf("replay did not reproduce observed endpoint: %+v %v", result, err)
		}
		if result.Savings != nil && math.Abs(*result.Savings) > .03 {
			t.Fatal("manual policy misattributed as optimizer saving", result)
		}
		readings[1].Source = ""
		result, err = ReplayObserved(readings)
		if err != nil || result.Comparable || result.Replayed != nil {
			t.Fatal("unproven remote source replayed", result, err)
		}
		readings[1].Source = "manual_override"
		readings = append(readings[:2], readings[8:]...)
		result, err = ReplayObserved(readings)
		if err != nil || result.Comparable || result.Reason != "telemetry_gap" {
			t.Fatal("gap hidden", result, err)
		}
	}
}
