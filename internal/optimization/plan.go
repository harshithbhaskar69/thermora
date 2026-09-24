// Package optimization compares transparent controllers against the same thermal model and inputs.
package optimization

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/scenario"
	"thermora.local/thermora/internal/sim"
)

type Interval struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Price   *float64  `json:"price_eur_per_kwh"`
	Outdoor *float64  `json:"forecast_outdoor_c"`
	Room    float64   `json:"room_target_c"`
	Reason  string    `json:"reason_code"`
}
type Outcome struct {
	TerminalBuffer   float64  `json:"terminal_buffer_c,omitempty"`
	TerminalHotWater float64  `json:"terminal_hot_water_c,omitempty"`
	DHWHeating       bool     `json:"terminal_dhw_heating"`
	Energy           float64  `json:"energy_kwh"`
	Cost             *float64 `json:"cost_eur"`
	Comfort          float64  `json:"comfort_degree_hours"`
	TerminalIndoor   float64  `json:"terminal_indoor_c"`
	TerminalSupply   float64  `json:"terminal_supply_c"`
	CompressorOn     bool     `json:"terminal_compressor_on"`
	DwellRemaining   uint32   `json:"terminal_dwell_remaining_sim_seconds"`
	HorizonEnergy    float64  `json:"horizon_energy_kwh"`
	HorizonCost      *float64 `json:"horizon_cost_eur"`
}
type Trace struct {
	At              time.Time `json:"at"`
	BaselineIndoor  float64   `json:"baseline_indoor_c"`
	OptimizedIndoor float64   `json:"optimized_indoor_c"`
	BaselineEnergy  float64   `json:"baseline_energy_kwh"`
	OptimizedEnergy float64   `json:"optimized_energy_kwh"`
}
type Plan struct {
	Preset            string           `json:"preset"`
	InitialBuffer     float64          `json:"initial_buffer_c,omitempty"`
	InitialHotWater   float64          `json:"initial_hot_water_c,omitempty"`
	Scenario          string           `json:"scenario"`
	Tariff            string           `json:"tariff"`
	InputHash         string           `json:"input_hash"`
	ModelVersion      string           `json:"model_version"`
	ControllerVersion string           `json:"controller_version"`
	Provenance        string           `json:"provenance"`
	Start             time.Time        `json:"start"`
	End               time.Time        `json:"end"`
	RecoveryEnd       time.Time        `json:"recovery_end"`
	InitialIndoor     float64          `json:"initial_indoor_c"`
	InitialSupply     float64          `json:"initial_supply_c"`
	Comfort           protocol.Comfort `json:"comfort"`
	Adopted           bool             `json:"adopted"`
	Reason            string           `json:"reason_code"`
	Comparable        bool             `json:"comparable"`
	Savings           *float64         `json:"savings_eur"`
	ComparisonReason  string           `json:"comparison_reason"`
	Baseline          Outcome          `json:"baseline"`
	Optimized         Outcome          `json:"optimized"`
	Intervals         []Interval       `json:"intervals"`
	Trace             []Trace          `json:"trace"`
}

// Build evaluates 24 hours plus six common-baseline recovery hours from equivalent initial states.
// The initial compressor is off with a full five-minute dwell: this is an explicit model assumption.
func Build(name, tariff string, start time.Time, comfort protocol.Comfort, indoor, supply float64) (Plan, error) {
	initial := sim.NewState(1)
	initial.Indoor, initial.Supply = indoor, supply
	return BuildForState(name, tariff, start, comfort, initial, "balanced")
}

// BuildForState compares the selected thermal model and policy with an identical baseline.
func BuildForState(
	name, tariff string,
	start time.Time,
	comfort protocol.Comfort,
	initial sim.State,
	preset string,
) (Plan, error) {
	indoor, supply := initial.Indoor, initial.Supply
	if preset == "" {
		preset = "balanced"
	}
	if preset != "comfort" && preset != "balanced" && preset != "maximum-savings" {
		return Plan{}, errors.New("unsupported preset")
	}

	code, ok := scenario.Code(name)
	if !ok || (tariff != "fixed" && tariff != "variable") || comfort.Validate() != nil || !finite(indoor, supply) ||
		indoor < 16 ||
		indoor > 26 ||
		supply < 16 ||
		supply > 60 ||
		start.Before(protocol.StartTime) ||
		start.Add(30*time.Hour).Sub(protocol.StartTime).Seconds() > math.MaxUint32 {
		return Plan{}, errors.New("invalid comparison inputs")
	}
	start = start.UTC().Truncate(10 * time.Second)
	p := Plan{
		Scenario:          name,
		Tariff:            tariff,
		Start:             start,
		End:               start.Add(24 * time.Hour),
		RecoveryEnd:       start.Add(30 * time.Hour),
		InitialIndoor:     indoor,
		InitialSupply:     supply,
		Comfort:           comfort,
		ModelVersion:      "zone-supply-v1",
		ControllerVersion: "quartile-v1",
		Provenance:        "modelled_comparison",
		Reason:            "price_schedule",
		Intervals:         []Interval{},
		Trace:             []Trace{},
	}
	p.Preset = preset
	if initial.Model == "heating-dhw" {
		p.ModelVersion = "zone-buffer-dhw-v1"
		p.InitialBuffer = initial.Buffer
		p.InitialHotWater = initial.Tank
	}
	margin := .25
	if preset == "maximum-savings" {
		margin = .05
	}
	prices := []float64{}
	complete := true
	for at := start; at.Before(p.RecoveryEnd); at = at.Add(15 * time.Minute) {
		price, forecast, _ := scenario.Inputs(code, at)
		if tariff == "fixed" {
			price = new(.15)
		}
		if price == nil || forecast == nil {
			complete = false
		}
		if at.Before(p.End) {
			p.Intervals = append(
				p.Intervals,
				Interval{
					Start:   at,
					End:     at.Add(15 * time.Minute),
					Price:   price,
					Outdoor: forecast,
					Room:    comfort.Target,
					Reason:  "normal",
				},
			)
			if price != nil {
				prices = append(prices, *price)
			}
		}
	}
	if complete && tariff == "variable" {
		sort.Float64s(prices)
		low, high := prices[len(prices)/4], prices[3*len(prices)/4]
		for i := range p.Intervals {
			s := &p.Intervals[i]
			if *s.Price >= high && high > low {
				s.Room = comfort.Min + margin
				if preset == "comfort" {
					s.Room = comfort.Target
				}
				s.Reason = "expensive_interval"
			}
			if *s.Price <= low && high > low {
				for j := i + 1; j < min(i+13, len(p.Intervals)); j++ {
					if *p.Intervals[j].Price >= high {
						s.Room = comfort.Max - margin
						if preset == "comfort" {
							s.Room = comfort.Target
						}
						s.Reason = "cheap_before_peak"
						break
					}
				}
			}
		}
	} else if !complete {
		p.Reason = "fallback_missing_input"
	} else {
		p.Reason = "weather_baseline_fixed_tariff"
	}
	p.Baseline, p.Optimized, p.Trace = evaluate(p, code, initial)
	pricesComplete := p.Baseline.Cost != nil && p.Optimized.Cost != nil
	comfortOK := p.Optimized.Comfort <= p.Baseline.Comfort+.0001
	terminalOK := math.Abs(p.Optimized.TerminalIndoor-p.Baseline.TerminalIndoor) <= .1 &&
		math.Abs(p.Optimized.TerminalSupply-p.Baseline.TerminalSupply) <= .5 &&
		p.Optimized.CompressorOn == p.Baseline.CompressorOn &&
		p.Optimized.DwellRemaining == 0 &&
		p.Baseline.DwellRemaining == 0
	if initial.Model == "heating-dhw" {
		terminalOK = terminalOK && math.Abs(p.Optimized.TerminalBuffer-p.Baseline.TerminalBuffer) <= .5 &&
			math.Abs(p.Optimized.TerminalHotWater-p.Baseline.TerminalHotWater) <= .5 &&
			p.Optimized.DHWHeating == p.Baseline.DHWHeating
	}
	p.Comparable = complete && pricesComplete && comfortOK && terminalOK
	p.ComparisonReason = "equivalent_terminal_states"
	if !terminalOK {
		p.ComparisonReason = "terminal_states_differ"
	}
	if !comfortOK {
		p.ComparisonReason = "additional_comfort_violation"
	}
	if !pricesComplete {
		p.ComparisonReason = "incomplete_prices"
	}
	if !complete && pricesComplete {
		p.ComparisonReason = "incomplete_forecast"
	}
	// Operational price shifting does not establish comparable savings. Equipment
	// protections remain authoritative; only the strict terminal gate permits a claim.
	p.Adopted = complete && tariff == "variable" && comfortOK && pricesComplete &&
		p.Optimized.TerminalIndoor >= p.Baseline.TerminalIndoor-.1 &&
		*p.Optimized.Cost < *p.Baseline.Cost-.000001
	if p.Comparable {
		p.Savings = new(*p.Baseline.Cost - *p.Optimized.Cost)
	}
	if !p.Adopted {
		if p.Reason == "price_schedule" {
			p.Reason = "fallback_constraints_or_cost"
		}
		for i := range p.Intervals {
			p.Intervals[i].Room = comfort.Target
			p.Intervals[i].Reason = p.Reason
		}
		// Preserve the rejected candidate's outcome for inspection; savings is never a claim about it.
	}
	data, err := json.Marshal(struct {
		Name, Tariff, Model, Controller, Preset string
		Start                                   time.Time
		Comfort                                 protocol.Comfort
		Initial                                 sim.State
	}{name, tariff, p.ModelVersion, p.ControllerVersion, preset, start, comfort, initial})
	if err != nil {
		return p, err
	}
	hash := sha256.Sum256(data)
	p.InputHash = hex.EncodeToString(hash[:])
	return p, nil
}

func evaluate(p Plan, code uint16, initial sim.State) (Outcome, Outcome, []Trace) {
	states := [2]sim.State{initial, initial}
	for i := range states {
		states[i].Indoor = p.InitialIndoor
		states[i].Supply = p.InitialSupply
		states[i].Comfort = p.Comfort
		states[i].Elapsed = uint32(p.Start.Sub(protocol.StartTime).Seconds())
		states[i].Frame = protocol.Frame{}
		states[i].LastRenewal = time.Time{}
		states[i].EnergyWh = 0
		states[i].DHWEnergyWh = 0
		states[i].ThermalEnergyWh = 0
		// Preserve the input dwell age while aligning its simulation origin.
		age := uint32(0)
		if initial.Elapsed >= initial.LastSwitch {
			age = initial.Elapsed - initial.LastSwitch
		}
		age = min(age, states[i].Elapsed)
		states[i].LastSwitch = states[i].Elapsed - age
	}
	out := [2]Outcome{}
	costs := [2]float64{}
	complete := true
	trace := []Trace{}
	wall := time.Unix(1, 0)
	for at := p.Start; at.Before(p.RecoveryEnd); at = at.Add(10 * time.Second) {
		price, _, outdoor := scenario.Inputs(code, at)
		if p.Tariff == "fixed" {
			price = new(.15)
		}
		if price == nil {
			complete = false
		}
		index := int(at.Sub(p.Start) / (15 * time.Minute))
		for i := range states {
			s := &states[i]
			s.Outdoor = outdoor
			if i == 1 && p.Reason == "price_schedule" && index < len(p.Intervals) &&
				at.Sub(p.Start)%(15*time.Minute) == 0 {
				target := p.Intervals[index].Room
				var f protocol.Frame
				protocol.Put32(f[:2], s.Epoch)
				protocol.Put32(f[2:4], s.Elapsed/900+1)
				protocol.Put32(f[4:6], 1)
				protocol.Put32(f[6:8], 1)
				f[8] = protocol.Centi(target)
				f[9] = protocol.Centi(sim.BaselineSupply(s.Indoor, outdoor, target))
				f[10] = protocol.Centi(p.Comfort.Target)
				f[11] = protocol.Centi(p.Comfort.Min)
				f[12] = protocol.Centi(p.Comfort.Max)
				f[13] = 1
				f[14] = 10
				protocol.Put32(f[18:20], uint32(p.End.Sub(protocol.StartTime).Seconds()))
				// The model assumes timely delivery, unlike the separately observed integration path.
				s.Frame = f
				s.LastRenewal = wall
			} else if i == 0 ||
				index >= len(p.Intervals) {
				s.Frame = protocol.Frame{}
			}
			if i == 1 && index < len(p.Intervals) {
				s.LastRenewal = wall
			}
			s.Step(wall)
			out[i].Comfort += (math.Max(0, p.Comfort.Min-s.Indoor) + math.Max(0, s.Indoor-p.Comfort.Max)) * 10 / 3600
			if price != nil {
				costs[i] += s.ElectricalW * 10 / 3600000 * *price
			}
		}
		if at.Add(10 * time.Second).Equal(p.End) {
			for i := range states {
				out[i].HorizonEnergy = states[i].EnergyWh / 1000
				if complete {
					out[i].HorizonCost = new(costs[i])
				}
			}
		}
		if at.Add(10*time.Second).Sub(p.Start)%(15*time.Minute) == 0 {
			trace = append(
				trace,
				Trace{
					At:              at.Add(10 * time.Second),
					BaselineIndoor:  states[0].Indoor,
					OptimizedIndoor: states[1].Indoor,
					BaselineEnergy:  states[0].EnergyWh / 1000,
					OptimizedEnergy: states[1].EnergyWh / 1000,
				},
			)
		}
	}
	for i, s := range states {
		out[i].Energy = s.EnergyWh / 1000
		if s.Model == "heating-dhw" {
			out[i].TerminalBuffer = s.Buffer
			out[i].TerminalHotWater = s.Tank
			out[i].DHWHeating = s.DHW
		}
		out[i].TerminalIndoor = s.Indoor
		out[i].TerminalSupply = s.Supply
		out[i].CompressorOn = s.On
		if s.Elapsed-s.LastSwitch < 300 {
			out[i].DwellRemaining = 300 - (s.Elapsed - s.LastSwitch)
		}
		if complete {
			out[i].Cost = new(costs[i])
		}
	}
	return out[0], out[1], trace
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
