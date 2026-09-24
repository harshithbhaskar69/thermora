package cloud

import (
	"context"
	"fmt"
	"time"

	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/storage"
	api "thermora.local/thermora/internal/transport/http/generated"
)

func settingsView(d deviceRecord) api.DeviceSettings {
	preset := d.Config.Preset
	if preset == "" {
		preset = "balanced"
	}
	result := api.DeviceSettings{
		Version: int(d.Config.Version),
		Model:   "heating",
		Preferences: api.HeatingPreferences{
			Preset:           api.HeatingPreferencesPreset(preset),
			FlexibilityOptIn: d.Config.Automation.Flexibility,
		},
	}
	if d.Config.HotWater.Enabled {
		result.Model = "heating-dhw"
		result.Preferences.HotWater = &api.HotWaterThresholds{
			StartC: float32(d.Config.HotWater.Start),
			StopC:  float32(d.Config.HotWater.Stop),
		}
	}
	return result
}

func (a *App) GetDeviceSettings(
	ctx context.Context,
	r api.GetDeviceSettingsRequestObject,
) (api.GetDeviceSettingsResponseObject, error) {
	if _, err := a.device(ctx, r.DeviceId, request(ctx).userID); err != nil {
		return nil, err
	}
	d, err := loadDevice(ctx, a.DB, r.DeviceId, false)
	return api.GetDeviceSettings200JSONResponse(settingsView(d)), err
}

func (a *App) UpdateDeviceSettings(
	ctx context.Context,
	r api.UpdateDeviceSettingsRequestObject,
) (api.UpdateDeviceSettingsResponseObject, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	s := request(ctx)
	if _, err := a.device(ctx, r.DeviceId, s.userID); err != nil {
		return nil, err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer storage.Rollback(ctx, tx)
	d, err := loadDevice(ctx, tx, r.DeviceId, true)
	if err != nil {
		return nil, err
	}
	if r.Params.IfMatch != fmt.Sprintf(`"%d"`, d.Config.Version) {
		return nil, fail(409, "stale_version", "Reload device settings before saving.")
	}
	if r.Body.HotWater != nil {
		if !d.Config.HotWater.Enabled {
			return nil, fail(
				422,
				"unsupported_capability",
				"This equipment has no domestic hot-water tank. Start a heating and hot-water simulation to enable it.",
			)
		}
		d.Config.HotWater = protocol.HotWater{
			Enabled: true,
			Start:   float64(r.Body.HotWater.StartC),
			Stop:    float64(r.Body.HotWater.StopC),
		}
	}
	if !r.Body.FlexibilityOptIn {
		if err = excludeFlexibility(ctx, tx, d.ID, "owner_not_opted_in"); err != nil {
			return nil, err
		}
	}
	d.Config.Preset = string(r.Body.Preset)
	d.Config.Automation.Flexibility = r.Body.FlexibilityOptIn
	d.Config.Version++
	d.Config.Generation++
	d.Config.IssuedAt = time.Now().UTC()
	if err = d.Config.Validate(d.ID, d.Run); err != nil {
		return nil, fail(422, "invalid_preferences", err.Error())
	}
	if err = fenceDevice(ctx, tx, d); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE devices SET config=$2 WHERE id=$1`, d.ID, d.Config); err != nil {
		return nil, err
	}
	if err = queueConfig(ctx, tx, d); err != nil {
		return nil, err
	}
	if err = audit(ctx, tx, d.Org, s.userID, "device_settings_updated", d.ID, s.id); err != nil {
		return nil, err
	}
	return api.UpdateDeviceSettings202JSONResponse(settingsView(d)), tx.Commit(ctx)
}
