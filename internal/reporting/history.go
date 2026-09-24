// Package reporting evaluates recorded simulation evidence without inventing missing measurements.
package reporting

import (
	"errors"
	"math"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/scenario"
)

type Reading struct {
	Sample        protocol.Sample
	Configuration *protocol.Configuration
	Tariff        string
	Source        string
}

type Bucket struct {
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	CoveredSeconds float64   `json:"covered_seconds"`
	PricedSeconds  float64   `json:"priced_seconds"`
	Energy         float64   `json:"covered_energy_kwh"`
	KnownCost      float64   `json:"known_cost_eur"`
	Cost           *float64  `json:"cost_eur"`
	Indoor         *float64  `json:"indoor_mean_c"`
	Target         *float64  `json:"last_room_target_c"`
	Supply         *float64  `json:"last_supply_setpoint_c"`
	Comfort        float64   `json:"comfort_degree_hours"`
}

type History struct {
	RunID          string    `json:"run_id"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	Timezone       string    `json:"timezone"`
	Provenance     string    `json:"provenance"`
	CoveredSeconds float64   `json:"covered_seconds"`
	PricedSeconds  float64   `json:"priced_seconds"`
	GapSeconds     float64   `json:"gap_seconds"`
	Energy         float64   `json:"covered_energy_kwh"`
	KnownCost      float64   `json:"known_cost_eur"`
	Cost           *float64  `json:"cost_eur"`
	Comfort        float64   `json:"comfort_degree_hours"`
	Buckets        []Bucket  `json:"buckets"`
}

// Summarize integrates monotonic counter deltas only between good samples at most
// 180 simulated seconds apart. Boundary allocation assumes uniform power within a pair.
// Missing intervals and unknown historical tariff/configuration never become zero-cost energy.
func Summarize(readings []Reading, from, to time.Time, bucketMinutes int) (History, error) {
	h := History{Start: from.UTC(), End: to.UTC(), Provenance: "observed_in_simulation", Buckets: []Bucket{}}
	if !to.After(from) || to.Sub(from) > 7*24*time.Hour || (bucketMinutes != 15 && bucketMinutes != 60) {
		return h, errors.New("choose a positive window up to seven days and 15 or 60 minute intervals")
	}
	for at := from; at.Before(to); at = at.Add(time.Duration(bucketMinutes) * time.Minute) {
		end := at.Add(time.Duration(bucketMinutes) * time.Minute)
		if end.After(to) {
			end = to
		}
		h.Buckets = append(h.Buckets, Bucket{Start: at.UTC(), End: end.UTC()})
	}
	for i := 1; i < len(readings); i++ {
		a, b := readings[i-1], readings[i]
		dt := b.Sample.ObservedAt.Sub(a.Sample.ObservedAt).Seconds()
		if dt <= 0 || dt > 180 || !usable(a.Sample) || !usable(b.Sample) || b.Sample.EnergyWh < a.Sample.EnergyWh {
			continue
		}
		start, end := a.Sample.ObservedAt, b.Sample.ObservedAt
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if !end.After(start) {
			continue
		}
		kwhPerSecond := float64(b.Sample.EnergyWh-a.Sample.EnergyWh) / 1000 / dt
		for at := start; at.Before(end); {
			index := int(at.Sub(from) / (time.Duration(bucketMinutes) * time.Minute))
			bucket := &h.Buckets[index]
			next := at.Truncate(15 * time.Minute).Add(15 * time.Minute)
			if next.After(end) {
				next = end
			}
			if next.After(bucket.End) {
				next = bucket.End
			}
			seconds := next.Sub(at).Seconds()
			energy := kwhPerSecond * seconds
			bucket.CoveredSeconds += seconds
			bucket.Energy += energy
			if bucket.Indoor == nil {
				bucket.Indoor = new(float64)
			}
			*bucket.Indoor += a.Sample.Indoor * seconds
			bucket.Target = new(a.Sample.RoomTarget)
			bucket.Supply = new(a.Sample.SupplyTarget)
			if a.Configuration != nil && b.Configuration != nil && a.Sample.ConfigVersion == b.Sample.ConfigVersion {
				comfort := a.Configuration.Comfort
				bucket.Comfort += (math.Max(0, comfort.Min-a.Sample.Indoor) + math.Max(0, a.Sample.Indoor-comfort.Max)) * seconds / 3600
				price, _, _ := scenario.Inputs(a.Configuration.WeatherProfile, at)
				if a.Tariff == "fixed" {
					price = new(.15)
				}
				if price != nil && (a.Tariff == "fixed" || a.Tariff == "variable") {
					bucket.KnownCost += energy * *price
					bucket.PricedSeconds += seconds
				}
			}
			at = next
		}
	}
	for i := range h.Buckets {
		b := &h.Buckets[i]
		if b.Indoor != nil {
			*b.Indoor /= b.CoveredSeconds
		}
		if math.Abs(b.PricedSeconds-b.End.Sub(b.Start).Seconds()) < .001 {
			b.Cost = new(b.KnownCost)
		}
		h.CoveredSeconds += b.CoveredSeconds
		h.PricedSeconds += b.PricedSeconds
		h.Energy += b.Energy
		h.KnownCost += b.KnownCost
		h.Comfort += b.Comfort
	}
	h.GapSeconds = to.Sub(from).Seconds() - h.CoveredSeconds
	if math.Abs(h.PricedSeconds-to.Sub(from).Seconds()) < .001 {
		h.Cost = new(h.KnownCost)
	}
	return h, nil
}

// DayWindow uses calendar arithmetic, so a DST day can contain 23 or 25 hours.
func DayWindow(day, zone string) (time.Time, time.Time, error) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, err := time.ParseInLocation("2006-01-02", day, location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start.UTC(), start.AddDate(0, 0, 1).UTC(), nil
}

func usable(s protocol.Sample) bool {
	for _, flag := range s.Quality {
		if flag != "buffered" {
			return false
		}
	}
	return true
}
