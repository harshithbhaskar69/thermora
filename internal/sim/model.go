// Package sim models equipment, independently of cloud control and transport.
package sim

import (
	"errors"
	"math"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/scenario"
)

// State is a complete restart checkpoint. All model durations use simulated seconds.
//
//nolint:recvcheck // Registers resolves a snapshot copy; pointer receivers mutate the live simulation.
type State struct {
	Buffer, Tank, DHWStart, DHWStop, DHWEnergyWh, ThermalEnergyWh float64
	DHW                                                           bool
	Paused                                                        bool
	Rate                                                          uint16
	RunFrame                                                      protocol.RunFrame
	Model                                                         string
	Epoch                                                         uint32
	Elapsed                                                       uint32
	Snapshot                                                      uint32
	Indoor, Outdoor, Supply, EnergyWh, ElectricalW, ThermalW      float64
	On                                                            bool
	LastSwitch                                                    uint32
	Frame                                                         protocol.Frame
	LastRenewal                                                   time.Time
	Comfort                                                       protocol.Comfort
	RoomTarget, SupplyTarget                                      float64
	Remote                                                        bool
	Flags                                                         uint16
}

func NewState(epoch uint32) State {
	return State{
		Epoch:        epoch,
		Rate:         60,
		Model:        "heating",
		Buffer:       35,
		Tank:         50,
		DHWStart:     45,
		DHWStop:      50,
		Indoor:       21,
		Outdoor:      -3,
		Supply:       30,
		Comfort:      protocol.Comfort{Target: 21, Min: 20, Max: 22},
		RoomTarget:   21,
		SupplyTarget: 40.6,
	}
}

func BaselineSupply(indoor, outdoor, target float64) float64 {
	return clamp(35+.7*(5-outdoor)+3*(target-indoor), 25, 50)
}

func (s *State) Apply(f protocol.Frame, now time.Time) error {
	if err := f.Validate(s.Epoch, s.Elapsed); err != nil {
		return err
	}
	seq, previous := protocol.Uint32(f[2:4]), protocol.Uint32(s.Frame[2:4])
	if seq < previous || (seq == previous && f != s.Frame) || protocol.Uint32(f[4:6]) < protocol.Uint32(s.Frame[4:6]) ||
		protocol.Uint32(f[6:8]) < protocol.Uint32(s.Frame[6:8]) {
		return errors.New("replayed or conflicting command")
	}
	if (f[22] == 1) != (s.Model == "heating-dhw") {
		return errors.New("equipment hot water capability mismatch")
	}
	s.Frame, s.LastRenewal = f, now
	if f[22] == 1 {
		s.DHWStart, s.DHWStop = protocol.Celsius(f[20]), protocol.Celsius(f[21])
	}
	if seq != previous {
		s.Snapshot++
	}
	s.Comfort = protocol.Comfort{
		Target: protocol.Celsius(f[10]),
		Min:    protocol.Celsius(f[11]),
		Max:    protocol.Celsius(f[12]),
	}
	s.resolve(now)
	return nil
}

func (s *State) resolve(now time.Time) {
	s.Flags = 0
	s.Remote = s.Frame[13] == 1 && now.Sub(s.LastRenewal) < 10*time.Second &&
		s.Elapsed < protocol.Uint32(s.Frame[18:20])
	s.RoomTarget = s.Comfort.Target
	s.SupplyTarget = BaselineSupply(s.Indoor, s.Outdoor, s.RoomTarget)
	if s.Remote {
		s.RoomTarget, s.SupplyTarget = protocol.Celsius(s.Frame[8]), protocol.Celsius(s.Frame[9])
	} else if s.Frame[13] == 1 {
		s.Flags |= 1 << 3
	}
	if s.Indoor < s.Comfort.Min {
		s.Flags |= 1
		s.RoomTarget = s.Comfort.Target
		s.SupplyTarget = 50
	}
	if s.Indoor > s.Comfort.Max {
		s.Flags |= 1 << 1
		s.RoomTarget = s.Comfort.Target
	}
	want := s.wantsHeat()
	if s.Model == "heating-dhw" {
		waterDemand := s.Tank < s.DHWStart || (s.DHW && s.Tank < s.DHWStop)
		want = want || (waterDemand && (s.Indoor >= s.Comfort.Min || s.Tank < 40))
		if s.Tank >= 60 || s.Buffer >= 60 {
			s.Flags |= 1 << 4
			return // Overheating bypasses minimum dwell; readback must preserve the alarm.
		}
	}
	if want != s.On && s.Elapsed-s.LastSwitch < 300 {
		s.Flags |= 1 << 2
	}
}

func (s State) wantsHeat() bool {
	wantOn := s.On
	if s.Indoor < s.RoomTarget-.2 {
		wantOn = true
	}
	if s.Indoor > s.RoomTarget+.2 || s.Indoor > s.Comfort.Max {
		wantOn = false
	}
	return wantOn
}

// Step advances exactly ten simulated seconds. Wall time only gates the equipment watchdog.
func (s *State) Step(now time.Time) {
	if s.Frame[15] != 0 {
		_, _, s.Outdoor = scenario.Inputs(s.Frame[15], protocol.StartTime.Add(time.Duration(s.Elapsed)*time.Second))
	}
	s.resolve(now)
	if s.Model == "heating-dhw" {
		s.stepHydronic()
		s.Elapsed += 10
		s.Snapshot++
		return
	}
	wantOn := s.wantsHeat()
	if wantOn != s.On {
		if s.Elapsed-s.LastSwitch >= 300 {
			s.On = wantOn
			s.LastSwitch = s.Elapsed
		}
	}
	target := s.Indoor
	if s.On {
		target = s.SupplyTarget
	}
	s.Supply = target + (s.Supply-target)*math.Exp(-10.0/120)
	cop := clamp(3.5+.06*s.Outdoor-.035*(s.Supply-35), 1.5, 5)
	s.ElectricalW, s.ThermalW = 15, 0
	if s.On {
		s.ThermalW = 1000 * math.Min(3*cop, .35*math.Max(s.Supply-s.Indoor, 0))
		s.ElectricalW = s.ThermalW / cop
	}
	s.Indoor += (10.0 / 3600) * (s.ThermalW/1000 + .25 - .18*(s.Indoor-s.Outdoor)) / 6
	s.EnergyWh += s.ElectricalW * 10 / 3600
	s.ThermalEnergyWh += s.ThermalW * 10 / 3600
	s.Elapsed += 10
	s.Snapshot++
}

func (s State) Registers(now time.Time) []uint16 {
	s.resolve(now)
	r := make([]uint16, protocol.InputCount)
	r[0] = 2
	protocol.Put32(r[1:3], s.Epoch)
	protocol.Put32(r[3:5], s.Elapsed)
	r[5], r[6], r[7] = protocol.Centi(s.Indoor), protocol.Centi(s.Outdoor), protocol.Centi(s.Supply)
	r[8], r[9] = protocol.Centi(s.RoomTarget), protocol.Centi(s.SupplyTarget)
	protocol.Put32(r[10:12], uint32(math.Round(s.ElectricalW)))
	protocol.Put32(r[12:14], uint32(math.Round(s.ThermalW)))
	protocol.Put32(r[14:16], uint32(s.EnergyWh))
	if s.On {
		r[16] = 1
	}
	if s.Flags&(1<<2) != 0 {
		r[16] = 2
	}
	r[17] = s.Flags
	copy(r[18:20], s.Frame[2:4])
	copy(r[20:22], s.Frame[4:6])
	copy(r[22:24], s.Frame[6:8])
	protocol.Put32(r[24:26], s.Snapshot)
	if s.Remote {
		r[26] = 1
	}
	if s.On {
		r[37] = 1
	}
	protocol.Put32(r[38:40], s.LastSwitch)
	protocol.Put32(r[34:36], uint32(s.ThermalEnergyWh))
	if s.Model == "heating-dhw" {
		r[27] = protocol.Centi(s.Buffer)
		r[28] = protocol.Centi(s.Tank)
		r[29] = protocol.Centi(s.DHWStart)
		r[30] = protocol.Centi(s.DHWStop)
		r[36] = 1
		if s.DHW {
			r[31] = 1
		}
		protocol.Put32(r[32:34], uint32(s.DHWEnergyWh))
	}
	return r
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// stepHydronic conserves heat between a 100 L buffer, 200 L hot-water tank and the zone.
// This deliberately simple fixture does not certify hygiene or real-equipment protection.
func (s *State) stepHydronic() {
	const dt = 10.0 / 3600
	if s.Tank < s.DHWStart {
		s.DHW = true
	}
	if s.Tank >= s.DHWStop {
		s.DHW = false
	}
	// A cold room has priority over ordinary hot-water charging; a cold tank is protected locally.
	waterPriority := s.DHW && (s.Indoor >= s.Comfort.Min || s.Tank < 40)
	want := s.wantsHeat() || waterPriority
	if s.Tank >= 60 || s.Buffer >= 60 {
		want = false
		if s.On {
			s.On = false
			s.LastSwitch = s.Elapsed
		}
		s.Flags |= 1 << 4
	}
	if want != s.On && s.Elapsed-s.LastSwitch >= 300 {
		s.On = want
		s.LastSwitch = s.Elapsed
	}
	if want != s.On {
		s.Flags |= 1 << 2
	}
	supply := s.Indoor
	if s.On {
		if waterPriority {
			supply = 58
		} else {
			supply = s.SupplyTarget
		}
	}
	s.Supply = supply + (s.Supply-supply)*math.Exp(-10.0/120)
	cop := clamp(3.5+.06*s.Outdoor-.035*(s.Supply-35), 1.5, 5)
	s.ElectricalW, s.ThermalW = 15, 0
	bufferHeat, tankHeat := 0.0, 0.0
	if s.On {
		sink := s.Buffer
		if waterPriority {
			sink = s.Tank
		}
		s.ThermalW = 1000 * math.Min(3*cop, .8*math.Max(s.Supply-sink, 0))
		s.ElectricalW = math.Max(15, s.ThermalW/cop)
		if waterPriority {
			tankHeat = s.ThermalW / 1000
			s.DHWEnergyWh += s.ElectricalW * dt
		} else {
			bufferHeat = s.ThermalW / 1000
		}
	}
	roomHeat := .35 * math.Max(0, s.Buffer-s.Indoor)
	bufferLoss := .002 * (s.Buffer - s.Indoor)
	tankLoss := .002 * (s.Tank - s.Indoor)
	// Two deterministic twenty-minute draws, each delivering 0.8 kWh of useful hot water.
	minute := s.Elapsed / 60 % 1440
	draw := 0.0
	if (minute >= 420 && minute < 440) || (minute >= 1140 && minute < 1160) {
		draw = 2.4
	}
	s.Buffer += dt * (bufferHeat - roomHeat - bufferLoss) / .116
	s.Tank += dt * (tankHeat - tankLoss - draw) / .232
	s.Indoor += dt * (roomHeat + bufferLoss + tankLoss + .25 - .18*(s.Indoor-s.Outdoor)) / 6
	s.EnergyWh += s.ElectricalW * dt
	s.ThermalEnergyWh += s.ThermalW * dt
}
