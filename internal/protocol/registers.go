package protocol

import (
	"errors"
	"math"
	"time"
)

const InputCount, FrameCount = 40, 23

type Frame [FrameCount]uint16

func Uint32(r []uint16) uint32   { return uint32(r[0])<<16 | uint32(r[1]) }
func Put32(r []uint16, v uint32) { r[0], r[1] = uint16(v>>16), uint16(v&0xffff) }

func Celsius(v uint16) float64 {
	// #nosec G115 -- Modbus represents signed centi-degrees as a two's-complement 16-bit word.
	return float64(int16(v)) / 100
}

// Centi encodes a temperature already bounded to the simulated equipment's range.
func Centi(v float64) uint16 {
	// #nosec G115 -- preserve the signed centi-degree bit pattern in an unsigned Modbus word.
	return uint16(int16(math.Round(v * 100)))
}

func (c Command) Frame(cfg Configuration) Frame {
	var f Frame
	Put32(f[0:2], c.RunEpoch)
	Put32(f[2:4], c.Sequence)
	Put32(f[4:6], c.Generation)
	Put32(f[6:8], c.ConfigVersion)
	f[8], f[9] = Centi(c.Parameters.Room), Centi(c.Parameters.Supply)
	f[10], f[11], f[12] = Centi(cfg.Comfort.Target), Centi(cfg.Comfort.Min), Centi(cfg.Comfort.Max)
	if c.Parameters.Mode == "remote" {
		f[13] = 1
	}
	f[14] = 10
	f[15] = cfg.WeatherProfile
	Put32(f[16:18], uint32(c.ValidFrom.Sub(StartTime).Seconds()))
	Put32(f[18:20], uint32(c.ValidUntil.Sub(StartTime).Seconds()))
	if cfg.HotWater.Enabled {
		f[20], f[21], f[22] = Centi(cfg.HotWater.Start), Centi(cfg.HotWater.Stop), 1
	}
	return f
}

func (f Frame) Validate(epoch, elapsed uint32) error {
	if f[22] > 1 || (f[22] == 0 && (f[20] != 0 || f[21] != 0)) {
		return errors.New("invalid hot water frame")
	}
	if f[22] == 1 {
		water := HotWater{Enabled: true, Start: Celsius(f[20]), Stop: Celsius(f[21])}
		if err := water.Validate(); err != nil {
			return err
		}
	}

	c := Comfort{Target: Celsius(f[10]), Min: Celsius(f[11]), Max: Celsius(f[12])}
	if err := c.Validate(); err != nil {
		return err
	}
	validOrdering := Uint32(f[:2]) == epoch && Uint32(f[2:4]) != 0 &&
		Uint32(f[4:6]) != 0 && Uint32(f[6:8]) != 0
	validRoomTarget := Celsius(f[8]) >= c.Min && Celsius(f[8]) <= c.Max
	validSupplyTarget := Celsius(f[9]) >= 25 && Celsius(f[9]) <= 50
	supportedControl := f[13] <= 1 && f[14] == 10 && f[15] <= 4
	validWindow := Uint32(f[16:18]) <= elapsed && Uint32(f[18:20]) > elapsed
	if !validOrdering || !validRoomTarget || !validSupplyTarget || !supportedControl || !validWindow {
		return errors.New("invalid command frame")
	}
	return nil
}

func SampleFromRegisters(r []uint16, now time.Time) (Sample, error) {
	if len(r) != InputCount || r[0] != 2 || r[16] > 3 || r[26] > 1 || r[31] > 1 || r[36] > 1 || r[37] > 1 {
		return Sample{}, errors.New("unsupported register snapshot")
	}
	s := Sample{
		Sequence:      Uint32(r[24:26]),
		Elapsed:       Uint32(r[3:5]),
		RecordedAt:    now.UTC(),
		Indoor:        Celsius(r[5]),
		Outdoor:       Celsius(r[6]),
		Supply:        Celsius(r[7]),
		RoomTarget:    Celsius(r[8]),
		SupplyTarget:  Celsius(r[9]),
		ElectricalW:   Uint32(r[10:12]),
		ThermalW:      Uint32(r[12:14]),
		EnergyWh:      Uint32(r[14:16]),
		Compressor:    []string{"off", "running", "dwell", "fault"}[r[16]],
		Mode:          []string{"baseline", "remote"}[r[26]],
		AppliedSeq:    Uint32(r[18:20]),
		ConfigVersion: Uint32(r[22:24]),
		Alarms:        []string{},
		Quality:       []string{},
	}
	s.CompressorOn = new(r[37] == 1)
	s.LastSwitch = new(Uint32(r[38:40]))
	s.ThermalEnergyWh = Uint32(r[34:36])
	if r[36] == 1 {
		s.Buffer = new(Celsius(r[27]))
		s.HotWater = &HotWaterSample{
			Temperature: Celsius(r[28]),
			Start:       Celsius(r[29]),
			Stop:        Celsius(r[30]),
			Heating:     r[31] == 1,
			EnergyWh:    Uint32(r[32:34]),
		}
	}
	s.ObservedAt = StartTime.Add(time.Duration(s.Elapsed) * time.Second)
	if r[17]&(1<<4) != 0 {
		s.Alarms = append(s.Alarms, "simulated_alarm")
	}
	if r[17]&(1<<5) != 0 {
		s.Quality = append(s.Quality, "sensor_fault")
	}
	return s, s.validate()
}
