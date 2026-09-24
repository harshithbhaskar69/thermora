package reporting

import (
	"math"
	"testing"
	"time"

	"thermora.local/thermora/internal/protocol"
)

func TestHistoryCounterIntegrationCoverageAndMissingPrices(t *testing.T) {
	t.Parallel()
	cfg := protocol.NewConfiguration("device", "run", protocol.Comfort{Target: 21, Min: 20, Max: 22})
	cfg.WeatherProfile = 2
	readings := []Reading{}
	for i := uint32(0); i <= 60; i++ {
		readings = append(
			readings,
			Reading{
				Sample: protocol.Sample{
					ObservedAt:    protocol.StartTime.Add(time.Duration(i) * time.Minute),
					EnergyWh:      i * 100,
					Indoor:        21,
					RoomTarget:    21,
					SupplyTarget:  40,
					ConfigVersion: 1,
				},
				Configuration: &cfg,
				Tariff:        "variable",
			},
		)
	}
	from, to := protocol.StartTime.Add(7*time.Minute), protocol.StartTime.Add(52*time.Minute)
	result, err := Summarize(readings, from, to, 15)
	if err != nil || result.Cost == nil || math.Abs(result.Energy-4.5) > 1e-9 || math.Abs(*result.Cost+.36) > 1e-9 ||
		result.GapSeconds != 0 {
		t.Fatalf("bad clipped counter integration: %+v %v", result, err)
	}
	// A missing observation interval remains visible; no interpolation across a long gap.
	gap := append(append([]Reading(nil), readings[:15]...), readings[20:]...)
	result, err = Summarize(gap, from, to, 15)
	if err != nil || result.Cost != nil || result.GapSeconds != 360 || result.CoveredSeconds != 2340 {
		t.Fatal("gap was fabricated as complete", result, err)
	}
	cfg.WeatherProfile = 3
	for i := range readings {
		readings[i].Sample.ObservedAt = readings[i].Sample.ObservedAt.Add(7 * time.Hour)
	}
	result, err = Summarize(readings, protocol.StartTime.Add(7*time.Hour), protocol.StartTime.Add(8*time.Hour), 60)
	if err != nil || result.Cost != nil || result.PricedSeconds != 0 || result.Energy < 5.99 {
		t.Fatal("missing tariff created cost", result, err)
	}
}

func TestCalendarDayDSTBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		day   string
		hours int
	}{{"2026-03-29", 23}, {"2026-10-25", 25}, {"2026-01-15", 24}} {
		start, end, err := DayWindow(tc.day, "Europe/Helsinki")
		if err != nil || end.Sub(start) != time.Duration(tc.hours)*time.Hour {
			t.Fatal("DST calendar boundary drift", tc, err)
		}
	}
	if _, _, err := DayWindow("2026-02-30", "Europe/Helsinki"); err == nil {
		t.Fatal("invalid date accepted")
	}
}
