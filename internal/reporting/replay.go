package reporting

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"thermora.local/thermora/internal/optimization"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/scenario"
	"thermora.local/thermora/internal/sim"
)

type Replay struct {
	ID               string                `json:"id"`
	DeviceID         string                `json:"device_id"`
	RunID            string                `json:"run_id"`
	CreatedAt        time.Time             `json:"created_at_wall"`
	Provenance       string                `json:"provenance"`
	InputHash        string                `json:"input_hash"`
	Start            time.Time             `json:"start"`
	End              time.Time             `json:"end"`
	RecoveryEnd      time.Time             `json:"recovery_end"`
	Ready            bool                  `json:"ready"`
	Comparable       bool                  `json:"comparable"`
	Reason           string                `json:"reason"`
	ObservedEnergy   float64               `json:"observed_energy_kwh"`
	ObservedCost     *float64              `json:"observed_cost_eur"`
	ModelEnergyError *float64              `json:"model_energy_error_kwh"`
	Savings          *float64              `json:"savings_eur"`
	Baseline         *optimization.Outcome `json:"baseline"`
	Replayed         *optimization.Outcome `json:"replayed"`
}

// ReplayObserved holds each observed policy until the next sample. It is a
// sampled-policy counterfactual, not a claim to reproduce unobserved transitions.
// Explicit manual interventions are applied to both trajectories. A full six-hour
// common-baseline recovery and agreement with observed endpoints gate any savings.
func ReplayObserved(readings []Reading) (Replay, error) {
	r := Replay{Provenance: "observed_policy_replay", Reason: "insufficient_evidence"}
	if len(readings) < 2 {
		return r, errors.New("record at least two samples before replaying")
	}
	first, last := readings[0].Sample, readings[len(readings)-1].Sample
	r.Start, r.End = first.ObservedAt, last.ObservedAt
	r.RecoveryEnd = r.End.Add(6 * time.Hour)
	if !r.End.After(r.Start) || r.End.Sub(r.Start) > 24*time.Hour {
		return r, errors.New("select a replay window up to 24 simulated hours")
	}
	data, err := json.Marshal(readings)
	if err != nil {
		return r, err
	}
	hash := sha256.Sum256(data)
	r.InputHash = hex.EncodeToString(hash[:])
	if last.EnergyWh < first.EnergyWh {
		return r, nil
	}
	r.ObservedEnergy = float64(last.EnergyWh-first.EnergyWh) / 1000
	history, err := Summarize(readings, r.Start, r.End, 60)
	if err != nil {
		return r, err
	}
	r.ObservedCost = history.Cost
	for i, v := range readings {
		s := v.Sample
		if s.Elapsed%10 != 0 || !s.ObservedAt.Equal(protocol.StartTime.Add(time.Duration(s.Elapsed)*time.Second)) {
			return r, nil
		}
		if !usable(s) || v.Configuration == nil || s.CompressorOn == nil || s.LastSwitch == nil ||
			(v.Tariff != "fixed" && v.Tariff != "variable") ||
			(s.Mode == "remote" && v.Source == "") {
			return r, nil
		}
		if i > 0 && (s.Elapsed <= readings[i-1].Sample.Elapsed || s.Elapsed-readings[i-1].Sample.Elapsed > 180) {
			r.Reason = "telemetry_gap"
			return r, nil
		}
		if (v.Configuration.HotWater.Enabled != (first.HotWater != nil)) || (s.HotWater != nil && s.Buffer == nil) ||
			(v.Configuration.HotWater.Enabled && (s.Buffer == nil || s.HotWater == nil)) {
			return r, nil
		}
	}
	initial := sim.NewState(1)
	initial.Elapsed = first.Elapsed
	initial.Indoor = first.Indoor
	initial.Supply = first.Supply
	initial.On = *first.CompressorOn
	initial.LastSwitch = *first.LastSwitch
	if first.HotWater != nil {
		initial.Model = "heating-dhw"
		initial.Tank = first.HotWater.Temperature
		initial.Buffer = *first.Buffer
		initial.DHW = first.HotWater.Heating
	}
	states := [2]sim.State{initial, initial}
	out := [2]optimization.Outcome{}
	costs := [2]float64{}
	priceComplete := true
	index := 0
	wall := time.Unix(1, 0)
	for at := r.Start; at.Before(r.RecoveryEnd); at = at.Add(10 * time.Second) {
		for index+1 < len(readings) && !readings[index+1].Sample.ObservedAt.After(at) {
			index++
		}
		input := readings[index]
		cfg := *input.Configuration
		price, _, outside := scenario.Inputs(cfg.WeatherProfile, at)
		if input.Tariff == "fixed" {
			price = new(.15)
		}
		if price == nil {
			priceComplete = false
		}
		for i := range states {
			s := &states[i]
			s.Outdoor = outside
			s.Comfort = cfg.Comfort
			if cfg.HotWater.Enabled {
				s.DHWStart, s.DHWStop = cfg.HotWater.Start, cfg.HotWater.Stop
			}
			s.Frame = protocol.Frame{}
			if at.Before(r.End) && input.Sample.Mode == "remote" && (i == 1 || input.Source == "manual_override") {
				supply := input.Sample.SupplyTarget
				if i == 0 {
					supply = sim.BaselineSupply(s.Indoor, s.Outdoor, input.Sample.RoomTarget)
				}
				cmd := protocol.Command{
					RunEpoch:      1,
					Sequence:      1,
					Generation:    cfg.Generation,
					ConfigVersion: cfg.Version,
					ValidFrom:     at,
					ValidUntil:    r.RecoveryEnd,
					Parameters:    protocol.Parameters{Mode: "remote", Room: input.Sample.RoomTarget, Supply: supply},
				}
				s.Frame = cmd.Frame(cfg)
				s.LastRenewal = wall
			}
			s.Step(wall)
			out[i].Comfort += (math.Max(0, cfg.Comfort.Min-s.Indoor) + math.Max(0, s.Indoor-cfg.Comfort.Max)) * 10 / 3600
			if price != nil {
				costs[i] += s.ElectricalW * 10 / 3600000 * *price
			}
			if at.Add(10 * time.Second).Equal(r.End) {
				out[i].HorizonEnergy = s.EnergyWh / 1000
				if priceComplete {
					out[i].HorizonCost = new(costs[i])
				}
				if i == 1 {
					r.ModelEnergyError = new(math.Abs(out[i].HorizonEnergy - r.ObservedEnergy))
					r.Ready = math.Abs(s.Indoor-last.Indoor) <= .25 && math.Abs(s.Supply-last.Supply) <= 1 &&
						*r.ModelEnergyError <= math.Max(.02, r.ObservedEnergy*.05)
					if last.HotWater != nil {
						r.Ready = r.Ready && math.Abs(s.Tank-last.HotWater.Temperature) <= 1 &&
							math.Abs(s.Buffer-*last.Buffer) <= 1
					}
				}
			}
		}
	}
	for i, s := range states {
		out[i].Energy = s.EnergyWh / 1000
		out[i].TerminalIndoor = s.Indoor
		out[i].TerminalSupply = s.Supply
		out[i].CompressorOn = s.On
		if s.Model == "heating-dhw" {
			out[i].TerminalBuffer = s.Buffer
			out[i].TerminalHotWater = s.Tank
			out[i].DHWHeating = s.DHW
		}
		if s.Elapsed-s.LastSwitch < 300 {
			out[i].DwellRemaining = 300 - (s.Elapsed - s.LastSwitch)
		}
		if priceComplete {
			out[i].Cost = new(costs[i])
		}
	}
	r.Baseline, r.Replayed = &out[0], &out[1]
	r.Reason = "sampled_policy_replay_mismatch"
	if r.Ready {
		r.Reason = "terminal_states_differ"
	}
	terminal := math.Abs(out[0].TerminalIndoor-out[1].TerminalIndoor) <= .1 &&
		math.Abs(out[0].TerminalSupply-out[1].TerminalSupply) <= .5 &&
		math.Abs(out[0].TerminalBuffer-out[1].TerminalBuffer) <= .5 &&
		math.Abs(out[0].TerminalHotWater-out[1].TerminalHotWater) <= .5 &&
		out[0].CompressorOn == out[1].CompressorOn &&
		out[0].DHWHeating == out[1].DHWHeating &&
		out[0].DwellRemaining == 0 &&
		out[1].DwellRemaining == 0
	r.Comparable = r.Ready && terminal && priceComplete && history.Cost != nil && out[1].Comfort <= out[0].Comfort+.0001
	if !priceComplete || history.Cost == nil {
		r.Reason = "incomplete_prices_or_coverage"
	}
	if out[1].Comfort > out[0].Comfort+.0001 {
		r.Reason = "additional_comfort_violation"
	}
	if r.Comparable {
		r.Savings = new(costs[0] - costs[1])
		r.Reason = "comparable_sampled_policy_model"
	}
	return r, nil
}
