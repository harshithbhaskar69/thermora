package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"thermora.local/thermora/internal/protocol"
	api "thermora.local/thermora/internal/transport/http/generated"
)

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type deviceRecord struct {
	ID, Org, Building, Name, Enrollment, Run string
	Epoch, Sequence, Accepted, Reported      uint32
	Config                                   protocol.Configuration
	Latest                                   *protocol.Sample
	Received                                 *time.Time
	OverrideTarget                           *float64
	OverrideUntil                            *time.Time
}

func loadDevice(ctx context.Context, q queryer, id string, lock bool) (deviceRecord, error) {
	var d deviceRecord
	var cfg, latest []byte
	query := `SELECT id,organization_id,COALESCE(building_id::text,''),name,enrollment,run_id,run_epoch,command_seq,accepted_version,reported_version,config,latest,last_received_at,override_target,override_until FROM devices WHERE id=$1`
	if lock {
		query += " FOR UPDATE"
	}
	err := q.QueryRow(
		ctx,
		query,
		id,
	).Scan(
		&d.ID,
		&d.Org,
		&d.Building,
		&d.Name,
		&d.Enrollment,
		&d.Run,
		&d.Epoch,
		&d.Sequence,
		&d.Accepted,
		&d.Reported,
		&cfg,
		&latest,
		&d.Received,
		&d.OverrideTarget,
		&d.OverrideUntil,
	)
	if err != nil {
		return d, err
	}
	if len(cfg) > 0 {
		if err = json.Unmarshal(cfg, &d.Config); err != nil {
			return d, err
		}
	}
	if len(latest) > 0 {
		if err = json.Unmarshal(latest, &d.Latest); err != nil {
			return d, err
		}
	}
	return d, nil
}

func (a *App) device(ctx context.Context, id, user string) (api.Device, error) {
	var visible bool
	err := a.DB.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM devices d JOIN buildings b ON b.id=d.building_id AND b.organization_id=d.organization_id WHERE d.id=$1 AND `+buildingAccess+`)`,
		id,
		user,
	).Scan(
		&visible,
	)
	if err != nil {
		return api.Device{}, err
	}
	if !visible {
		return api.Device{}, missing()
	}
	d, err := loadDevice(ctx, a.DB, id, false)
	if err != nil {
		return api.Device{}, err
	}
	v := deviceView(d)
	allowed, err := connected(ctx, a.DB, id)
	if err != nil {
		return v, err
	}
	v.ControlConnected = &allowed
	route, err := a.manufacturerRoute(ctx, id)
	if err == nil {
		v.ConnectionId = &route.Connection
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	return v, nil
}

func deviceView(d deviceRecord) api.Device {
	r := api.Device{
		Id:              d.ID,
		BuildingId:      d.Building,
		Name:            d.Name,
		Enrollment:      api.DeviceEnrollment(d.Enrollment),
		RunId:           d.Run,
		DesiredVersion:  int(d.Config.Version),
		ReportedVersion: int(d.Reported),
		Generation:      int(d.Config.Generation),
		Origin:          api.Simulation,
		Connectivity:    api.Waiting,
		LastReceivedAt:  d.Received,
	}
	if d.Received != nil {
		age := time.Since(*d.Received)
		switch {
		case age < 15*time.Second:
			r.Connectivity = api.Online
		case age < 45*time.Second:
			r.Connectivity = api.Stale
		default:
			r.Connectivity = api.Offline
		}
	}
	if d.Latest != nil {
		s := sampleView(*d.Latest)
		r.Latest = &s
	}
	return r
}

func sampleView(s protocol.Sample) api.Sample {
	quality := make([]api.SampleQualityFlags, len(s.Quality))
	for i, flag := range s.Quality {
		quality[i] = api.SampleQualityFlags(flag)
	}
	return api.Sample{
		CompressorOn: s.CompressorOn, LastSwitchSimSeconds: func() *int {
			if s.LastSwitch == nil {
				return nil
			}
			return new(int(*s.LastSwitch))
		}(),
		BufferTemperatureC: s.Buffer, HotWater: s.HotWater, ThermalEnergyWh: new(int(s.ThermalEnergyWh)),
		SampleSeq: int(s.Sequence), SimElapsedSeconds: int(s.Elapsed),
		ObservedAt: s.ObservedAt, RecordedAtWall: s.RecordedAt,
		IndoorTemperatureC: s.Indoor, OutdoorTemperatureC: s.Outdoor, SupplyTemperatureC: s.Supply,
		RoomTargetC: s.RoomTarget, SupplySetpointC: s.SupplyTarget,
		ElectricalPowerW: int(s.ElectricalW), ThermalPowerW: int(s.ThermalW), ElectricalEnergyWh: int(s.EnergyWh),
		CompressorState: api.SampleCompressorState(s.Compressor), ControlMode: api.SampleControlMode(s.Mode),
		AlarmCodes: s.Alarms, QualityFlags: quality,
		AppliedCommandSeq: int(s.AppliedSeq), ReportedConfigVersion: int(s.ConfigVersion),
	}
}

func (a *App) GetDevice(ctx context.Context, r api.GetDeviceRequestObject) (api.GetDeviceResponseObject, error) {
	d, err := a.device(ctx, r.DeviceId, request(ctx).userID)
	return api.GetDevice200JSONResponse(d), err
}

func (a *App) GetSeries(ctx context.Context, r api.GetSeriesRequestObject) (api.GetSeriesResponseObject, error) {
	d, err := a.device(ctx, r.DeviceId, request(ctx).userID)
	if err != nil {
		return nil, err
	}
	rows, err := a.DB.Query(
		ctx,
		`SELECT sample FROM (SELECT sample,elapsed FROM telemetry WHERE device_id=$1 AND run_id=$2 ORDER BY elapsed DESC LIMIT 600) recent ORDER BY elapsed`,
		d.Id,
		d.RunId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []api.Sample{}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var sample protocol.Sample
		if err = json.Unmarshal(data, &sample); err != nil {
			return nil, err
		}
		result = append(result, sampleView(sample))
	}
	return api.GetSeries200JSONResponse(result), rows.Err()
}
