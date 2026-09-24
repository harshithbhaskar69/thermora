// Package protocol defines Thermora's versioned edge messages and simulated register contract.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
)

const MaxMessageBytes = 64 * 1024

var StartTime = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

type Comfort struct {
	Target float64 `json:"target_c"`
	Min    float64 `json:"min_c"`
	Max    float64 `json:"max_c"`
}

func (c Comfort) Validate() error {
	if !finite(c.Min, c.Max, c.Target) || c.Min < 16 || c.Max > 26 || c.Max-c.Min < .5 || c.Target < c.Min ||
		c.Target > c.Max {
		return errors.New("comfort requires 16 ≤ min < max ≤ 26, a range of at least 0.5 °C, and a target inside it")
	}
	return nil
}

type HotWater struct {
	Enabled bool    `json:"enabled"`
	Start   float64 `json:"start_c"`
	Stop    float64 `json:"stop_c"`
}

func (h HotWater) Validate() error {
	if !h.Enabled && h.Start == 0 && h.Stop == 0 {
		return nil
	}
	if !finite(h.Start, h.Stop) || h.Start < 40 || h.Stop > 55 || h.Stop-h.Start < 3 {
		return errors.New("hot water requires 40 ≤ start < stop ≤ 55 with at least 3 °C hysteresis")
	}
	return nil
}

type Configuration struct {
	Preset         string    `json:"preset,omitempty"`
	HotWater       HotWater  `json:"hot_water"`
	WeatherProfile uint16    `json:"weather_profile,omitempty"`
	SchemaVersion  int       `json:"schema_version"`
	DeviceID       string    `json:"device_id"`
	RunID          string    `json:"run_id"`
	Version        uint32    `json:"config_version"`
	Generation     uint32    `json:"generation"`
	IssuedAt       time.Time `json:"issued_at_wall"`
	Comfort        Comfort   `json:"comfort"`
	Automation     struct {
		Optimization bool `json:"optimization_enabled"`
		Flexibility  bool `json:"flexibility_opt_in"`
	} `json:"automation"`
	CurveVersion   string `json:"baseline_curve_version"`
	ModelVersion   string `json:"model_version"`
	SampleInterval uint32 `json:"sample_interval_sim_seconds"`
	SupplyLimits   struct {
		Min float64 `json:"min_c"`
		Max float64 `json:"max_c"`
	} `json:"supply_limits"`
}

func NewConfiguration(device, run string, comfort Comfort) Configuration {
	c := Configuration{
		SchemaVersion:  1,
		DeviceID:       device,
		RunID:          run,
		Version:        1,
		Generation:     1,
		IssuedAt:       time.Now().UTC(),
		Comfort:        comfort,
		Preset:         "balanced",
		CurveVersion:   "weather-v1",
		ModelVersion:   "zone-supply-v1",
		SampleInterval: 60,
	}
	c.SupplyLimits.Min, c.SupplyLimits.Max = 25, 50
	return c
}

func (c Configuration) Validate(device, run string) error {
	identityMatches := c.SchemaVersion == 1 && c.DeviceID == device && c.RunID == run
	hasOrdering := c.Version != 0 && c.Generation != 0
	supportedModel := c.ModelVersion == "zone-supply-v1" || c.ModelVersion == "zone-buffer-dhw-v1"
	supportedBaseline := c.CurveVersion == "weather-v1" && c.SampleInterval == 60
	supportedSupplyLimits := c.SupplyLimits.Min == 25 && c.SupplyLimits.Max == 50
	validWeather := c.WeatherProfile <= 4 && (!c.Automation.Optimization || c.WeatherProfile != 0)
	if !identityMatches || !hasOrdering || !supportedModel || !supportedBaseline ||
		!supportedSupplyLimits || !validWeather {
		return errors.New("unsupported configuration")
	}
	if c.Preset != "" && c.Preset != "comfort" && c.Preset != "balanced" && c.Preset != "maximum-savings" {
		return errors.New("unsupported optimization preset")
	}
	if c.HotWater.Enabled != (c.ModelVersion == "zone-buffer-dhw-v1") {
		return errors.New("hot water capability mismatch")
	}
	return errors.Join(c.Comfort.Validate(), c.HotWater.Validate())
}

type ConfigReport struct {
	SchemaVersion int       `json:"schema_version"`
	DeviceID      string    `json:"device_id"`
	RunID         string    `json:"run_id"`
	Desired       uint32    `json:"desired_version"`
	Accepted      uint32    `json:"accepted_version"`
	Reported      uint32    `json:"reported_version"`
	Status        string    `json:"status"`
	ReportedAt    time.Time `json:"reported_at_wall"`
}

type Parameters struct {
	Mode   string  `json:"mode"`
	Room   float64 `json:"room_target_c"`
	Supply float64 `json:"supply_setpoint_c"`
}

type Command struct {
	SchemaVersion int        `json:"schema_version"`
	ID            string     `json:"command_id"`
	DeviceID      string     `json:"device_id"`
	RunID         string     `json:"run_id"`
	RunEpoch      uint32     `json:"run_epoch"`
	Sequence      uint32     `json:"command_seq"`
	Generation    uint32     `json:"generation"`
	ConfigVersion uint32     `json:"config_version"`
	IssuedAt      time.Time  `json:"issued_at_wall"`
	ExpiresAt     time.Time  `json:"expires_at_wall"`
	ValidFrom     time.Time  `json:"valid_from"`
	ValidUntil    time.Time  `json:"valid_until"`
	Kind          string     `json:"kind"`
	Source        string     `json:"source"`
	Reason        string     `json:"reason_code"`
	CorrelationID string     `json:"correlation_id"`
	Parameters    Parameters `json:"parameters"`
}

func (c Command) Validate(cfg Configuration, epoch, reported, elapsed uint32, now time.Time) error {
	if _, err := uuid.Parse(c.ID); err != nil {
		return errors.New("invalid_command_id")
	}
	if c.SchemaVersion != 1 || c.Kind != "set_heating_policy" || c.DeviceID != cfg.DeviceID || c.RunID != cfg.RunID ||
		c.RunEpoch != epoch {
		return errors.New("identity_mismatch")
	}
	if c.Sequence == 0 || c.ConfigVersion != cfg.Version || c.Generation != cfg.Generation {
		return errors.New("stale_configuration")
	}
	if !now.Before(c.ExpiresAt) || c.IssuedAt.After(now.Add(5*time.Second)) ||
		c.ExpiresAt.Sub(c.IssuedAt) > 30*time.Second ||
		!c.ExpiresAt.After(c.IssuedAt) {
		return errors.New("expired_command")
	}
	simNow := StartTime.Add(time.Duration(elapsed) * time.Second)
	if !c.ValidUntil.After(simNow) || !c.ValidUntil.After(c.ValidFrom) || c.ValidFrom.Before(StartTime) ||
		c.ValidUntil.Sub(StartTime).Seconds() > math.MaxUint32 {
		return errors.New("invalid_simulation_window")
	}
	p := c.Parameters
	if !finite(p.Room, p.Supply) || p.Room < cfg.Comfort.Min || p.Room > cfg.Comfort.Max || p.Supply < 25 ||
		p.Supply > 50 ||
		(p.Mode != "remote" && p.Mode != "baseline") {
		return errors.New("invalid_setpoint")
	}
	if p.Mode == "remote" && reported != cfg.Version {
		return errors.New("configuration_not_applied")
	}
	if p.Mode == "baseline" && c.Reason != "config_sync" && reported != cfg.Version {
		return errors.New("configuration_not_applied")
	}
	return nil
}

type Lease struct {
	SchemaVersion int       `json:"schema_version"`
	DeviceID      string    `json:"device_id"`
	RunID         string    `json:"run_id"`
	Generation    uint32    `json:"generation"`
	Sequence      uint32    `json:"command_seq"`
	IssuedAt      time.Time `json:"issued_at_wall"`
	ValidUntil    time.Time `json:"valid_until_wall"`
}

type Event struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"event_id"`
	CommandID     string    `json:"command_id"`
	DeviceID      string    `json:"device_id"`
	RunID         string    `json:"run_id"`
	Stage         string    `json:"stage"`
	Reason        string    `json:"reason_code,omitempty"`
	OccurredAt    time.Time `json:"occurred_at_wall"`
	ObservedAt    time.Time `json:"observed_at"`
}

type HotWaterSample struct {
	Temperature float64 `json:"temperature_c"`
	Start       float64 `json:"start_c"`
	Stop        float64 `json:"stop_c"`
	Heating     bool    `json:"heating"`
	EnergyWh    uint32  `json:"electrical_energy_wh"`
}

type Sample struct {
	CompressorOn    *bool           `json:"compressor_on,omitempty"`
	LastSwitch      *uint32         `json:"last_switch_sim_seconds,omitempty"`
	Buffer          *float64        `json:"buffer_temperature_c,omitempty"`
	HotWater        *HotWaterSample `json:"hot_water,omitempty"`
	ThermalEnergyWh uint32          `json:"thermal_energy_wh,omitempty"`
	Sequence        uint32          `json:"sample_seq"`
	Elapsed         uint32          `json:"sim_elapsed_seconds"`
	ObservedAt      time.Time       `json:"observed_at"`
	RecordedAt      time.Time       `json:"recorded_at_wall"`
	Indoor          float64         `json:"indoor_temperature_c"`
	Outdoor         float64         `json:"outdoor_temperature_c"`
	Supply          float64         `json:"supply_temperature_c"`
	RoomTarget      float64         `json:"room_target_c"`
	SupplyTarget    float64         `json:"supply_setpoint_c"`
	ElectricalW     uint32          `json:"electrical_power_w"`
	ThermalW        uint32          `json:"thermal_power_w"`
	EnergyWh        uint32          `json:"electrical_energy_wh"`
	Compressor      string          `json:"compressor_state"`
	Mode            string          `json:"control_mode"`
	Alarms          []string        `json:"alarm_codes"`
	AppliedSeq      uint32          `json:"applied_command_seq"`
	ConfigVersion   uint32          `json:"reported_config_version"`
	Quality         []string        `json:"quality_flags"`
}

type Batch struct {
	SchemaVersion int       `json:"schema_version"`
	DeviceID      string    `json:"device_id"`
	RunID         string    `json:"run_id"`
	BootID        string    `json:"boot_id"`
	ID            string    `json:"batch_id"`
	Origin        string    `json:"origin"`
	SentAt        time.Time `json:"sent_at_wall"`
	Samples       []Sample  `json:"samples"`
}

func (b Batch) Validate() error {
	for _, id := range []string{b.DeviceID, b.RunID, b.BootID, b.ID} {
		if _, err := uuid.Parse(id); err != nil {
			return errors.New("invalid_identity")
		}
	}
	if b.SchemaVersion != 1 || b.Origin != "simulation" || b.SentAt.IsZero() || len(b.Samples) == 0 ||
		len(b.Samples) > 100 {
		return errors.New("invalid_batch")
	}
	for _, s := range b.Samples {
		if s.Sequence == 0 || s.validate() != nil {
			return errors.New("invalid_sample")
		}
	}
	return nil
}

func (s Sample) validate() error {
	if (s.CompressorOn == nil) != (s.LastSwitch == nil) || (s.LastSwitch != nil && *s.LastSwitch > s.Elapsed) {
		return errors.New("invalid compressor timing")
	}
	if (s.Buffer == nil) != (s.HotWater == nil) {
		return errors.New("incomplete tank readings")
	}
	if s.HotWater != nil {
		h := s.HotWater
		if !finite(*s.Buffer, h.Temperature) || *s.Buffer < 0 || *s.Buffer > 90 || h.Temperature < 0 ||
			h.Temperature > 90 ||
			h.EnergyWh > s.EnergyWh ||
			(HotWater{true, h.Start, h.Stop}).Validate() != nil {
			return errors.New("invalid tank reading")
		}
	}
	if !finite(s.Indoor, s.Outdoor, s.Supply, s.RoomTarget, s.SupplyTarget) ||
		s.Indoor < -40 || s.Indoor > 80 || s.Outdoor < -60 || s.Outdoor > 60 || s.Supply < -40 || s.Supply > 90 ||
		s.RoomTarget < 16 || s.RoomTarget > 26 || s.SupplyTarget < 25 || s.SupplyTarget > 50 ||
		s.ElectricalW > 10000 || s.ThermalW > 50000 || s.RecordedAt.IsZero() ||
		!s.ObservedAt.Equal(StartTime.Add(time.Duration(s.Elapsed)*time.Second)) ||
		!slices.Contains([]string{"baseline", "remote"}, s.Mode) ||
		!slices.Contains([]string{"off", "running", "dwell", "fault"}, s.Compressor) ||
		s.Alarms == nil || s.Quality == nil || len(s.Alarms) > 16 || len(s.Quality) > 7 {
		return errors.New("invalid_sample")
	}
	for _, flag := range s.Quality {
		if !slices.Contains(
			[]string{
				"sensor_fault",
				"out_of_range",
				"missing",
				"clock_uncertain",
				"buffered",
				"counter_reset",
				"gap_before",
			},
			flag,
		) {
			return errors.New("invalid_quality_flag")
		}
	}
	for _, alarm := range s.Alarms {
		if len(alarm) == 0 || len(alarm) > 64 {
			return errors.New("invalid_alarm_code")
		}
	}
	return nil
}

type Receipt struct {
	SchemaVersion int       `json:"schema_version"`
	BatchID       string    `json:"batch_id,omitempty"`
	EventID       string    `json:"event_id,omitempty"`
	Status        string    `json:"status"`
	Code          string    `json:"code,omitempty"`
	ReceivedAt    time.Time `json:"received_at_wall"`
}

func Topic(org, device, suffix string) string {
	return fmt.Sprintf("thermora/v1/org/%s/device/%s/%s", org, device, suffix)
}

// Decode rejects oversized messages and trailing JSON; minor optional fields remain compatible.
func Decode(data []byte, dst any) error {
	if len(data) > MaxMessageBytes {
		return errors.New("message_too_large")
	}
	return json.Unmarshal(data, dst)
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
