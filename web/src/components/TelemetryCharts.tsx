import type { Building, Sample } from "../api/client";
import TimeChart, { type ChartRow } from "./TimeChart";
import { samplePoints, timeLabel } from "./chart-data";

export default function TelemetryCharts({
  samples,
  building,
}: {
  samples: Sample[];
  building: Building;
}) {
  if (!samples.length)
    return (
      <div className="chart-empty">
        <h3>No telemetry yet</h3>
        <p className="muted">
          The timeline starts with the first authenticated equipment sample.
        </p>
      </div>
    );
  const rows: ChartRow[] = [
    {
      unit: "Temperature · °C",
      comfort: [
        building.preferences.comfort_min_c,
        building.preferences.comfort_max_c,
      ],
      series: [
        {
          name: "Indoor temperature",
          values: samplePoints(samples, (s) => s.indoor_temperature_c),
          color: "indoor",
        },
        {
          name: "Room target",
          values: samplePoints(samples, (s) => s.room_target_c),
          color: "target",
          step: true,
        },
      ],
    },
    {
      unit: "Electrical power · kW",
      zero: true,
      series: [
        {
          name: "Electrical power",
          values: samplePoints(samples, (s) => s.electrical_power_w / 1000),
          color: "power",
          area: true,
        },
      ],
    },
  ];
  if (samples.some((s) => s.hot_water))
    rows.push({
      unit: "Water temperature · °C",
      series: [
        {
          name: "Hot water",
          values: samplePoints(samples, (s) => s.hot_water?.temperature_c),
          color: "target",
        },
        {
          name: "Buffer",
          values: samplePoints(samples, (s) => s.buffer_temperature_c),
          color: "outdoor",
        },
      ],
    });
  const zone = building.preferences.timezone;
  return (
    <>
      <TimeChart
        live
        rows={rows}
        timezone={zone}
        label="Indoor temperature, room target and electrical power over simulated time. Shading shows the comfort range; breaks are missing evidence."
      />
      <details>
        <summary>View data table · latest 20 samples</summary>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Simulated time</th>
                <th>Indoor °C</th>
                <th>Target °C</th>
                <th>Supply °C</th>
                <th>Power W</th>
                <th>Energy Wh</th>
              </tr>
            </thead>
            <tbody>
              {samples.slice(-20).map((s) => (
                <tr key={`${s.recorded_at_wall}-${s.sample_seq}`}>
                  <td>{timeLabel(s.observed_at, zone, true)}</td>
                  <td>{s.indoor_temperature_c.toFixed(2)}</td>
                  <td>{s.room_target_c.toFixed(1)}</td>
                  <td>{s.supply_temperature_c.toFixed(2)}</td>
                  <td>{s.electrical_power_w}</td>
                  <td>{s.electrical_energy_wh}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </>
  );
}
