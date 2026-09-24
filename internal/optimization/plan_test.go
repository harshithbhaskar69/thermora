package optimization

import (
	"math"
	"reflect"
	"testing"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/sim"
)

func TestEquivalentComparisonAndMissingInputs(t *testing.T) {
	comfort := protocol.Comfort{Target: 21, Min: 20, Max: 22}
	for _, name := range []string{"winter", "negative-price", "missing-price", "missing-forecast"} {
		t.Run(name, func(t *testing.T) {
			p, err := Build(name, "variable", protocol.StartTime, comfort, 21, 30)
			if err != nil {
				t.Fatal(err)
			}
			repeat, err := Build(name, "variable", protocol.StartTime, comfort, 21, 30)
			if err != nil || !reflect.DeepEqual(p, repeat) {
				t.Fatal("comparison was not deterministic", err)
			}
			if len(p.Intervals) != 96 || len(p.Trace) != 120 || p.RecoveryEnd.Sub(p.End) != 6*time.Hour {
				t.Fatal("incomplete evaluation/rebound window")
			}
			if p.Comparable &&
				(p.Savings == nil || p.Optimized.Comfort > p.Baseline.Comfort+.0001 ||
					math.Abs(p.Optimized.TerminalIndoor-p.Baseline.TerminalIndoor) > .1 ||
					math.Abs(p.Optimized.TerminalSupply-p.Baseline.TerminalSupply) > .5 ||
					p.Optimized.CompressorOn != p.Baseline.CompressorOn ||
					p.Optimized.DwellRemaining != 0 || p.Baseline.DwellRemaining != 0 ||
					math.Abs(*p.Savings-(*p.Baseline.Cost-*p.Optimized.Cost)) > 1e-9) {
				t.Fatal("invalid savings claim")
			}
			if !p.Comparable && p.Savings != nil {
				t.Fatal("incomparable terminal states produced a savings claim")
			}
			if p.Adopted && p.Optimized.TerminalIndoor < p.Baseline.TerminalIndoor-.1 {
				t.Fatal("controller ignored predicted indoor recovery constraint")
			}
			if (name == "winter" || name == "negative-price") && !p.Adopted {
				t.Fatal("known feasible fixture no longer shifts load")
			}
			if name == "missing-price" &&
				(p.Adopted || p.Baseline.Cost != nil || p.Optimized.Cost != nil || p.Savings != nil) {
				t.Fatal("missing prices fabricated a complete cost")
			}
			if name == "missing-forecast" && (p.Adopted || p.Reason != "fallback_missing_input") {
				t.Fatal("missing weather did not fail back")
			}
			if name == "negative-price" && (p.Intervals[0].Price == nil || *p.Intervals[0].Price >= 0) {
				t.Fatal("negative tariff was discarded")
			}
			for _, s := range p.Intervals {
				if s.Room < comfort.Min || s.Room > comfort.Max {
					t.Fatal("schedule escaped comfort bounds")
				}
			}
			t.Logf("adopted=%v reason=%s baseline=%+v candidate=%+v", p.Adopted, p.Reason, p.Baseline, p.Optimized)
			if p.Baseline.Cost != nil {
				t.Logf("cost baseline=%f candidate=%f", *p.Baseline.Cost, *p.Optimized.Cost)
			}
		})
	}
	_, err := Build("winter", "variable", protocol.StartTime, comfort, math.NaN(), 30)
	if err == nil {
		t.Fatal("NaN accepted")
	}
}

func TestDSTIntervalsAndFixedTariff(t *testing.T) {
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	p, err := Build("winter", "fixed", start, protocol.Comfort{Target: 21, Min: 20, Max: 22}, 21, 30)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range p.Intervals {
		if s.End.Sub(s.Start) != 15*time.Minute || s.Price == nil || *s.Price != .15 ||
			i > 0 && !p.Intervals[i-1].End.Equal(s.Start) {
			t.Fatal("tariff intervals are not continuous UTC")
		}
	}
	if p.Adopted || p.Reason != "weather_baseline_fixed_tariff" {
		t.Fatal("fixed tariff incorrectly uses price shifting")
	}
}

func TestHotWaterTerminalStateAndPresets(t *testing.T) {
	t.Parallel()
	initial := sim.NewState(1)
	initial.Model = "heating-dhw"
	var balanced Plan
	for _, preset := range []string{"comfort", "balanced", "maximum-savings"} {
		p, err := BuildForState("winter", "variable", protocol.StartTime, initial.Comfort, initial, preset)
		if err != nil {
			t.Fatal(err)
		}
		if p.ModelVersion != "zone-buffer-dhw-v1" || p.Baseline.TerminalHotWater == 0 ||
			p.Optimized.TerminalBuffer == 0 {
			t.Fatal("tank state omitted")
		}
		if p.Savings != nil &&
			(math.Abs(p.Baseline.TerminalHotWater-p.Optimized.TerminalHotWater) > .5 ||
				math.Abs(p.Baseline.TerminalBuffer-p.Optimized.TerminalBuffer) > .5) {
			t.Fatal("colder tanks produced savings")
		}
		if preset == "comfort" {
			for _, i := range p.Intervals {
				if i.Room != initial.Comfort.Target {
					t.Fatal("comfort preset shifted target")
				}
			}
		}
		if preset == "balanced" {
			balanced = p
		}
	}
	initial.Tank -= 1
	changed, err := BuildForState("winter", "variable", protocol.StartTime, initial.Comfort, initial, "balanced")
	if err != nil || changed.InputHash == balanced.InputHash {
		t.Fatal("tank initial state absent from input hash", err)
	}
}
