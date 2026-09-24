import { describe, expect, it } from "vitest";
import { historyWindow, samplePoints, timeLabel } from "./chart-data";
import type { Sample } from "../api/client";

const sample = (sequence: number, wallSeconds = sequence): Sample => ({
  sample_seq: sequence,
  observed_at: new Date(sequence * 60_000).toISOString(),
  recorded_at_wall: new Date(wallSeconds * 1000).toISOString(),
  quality_flags: [],
  indoor_temperature_c: 21,
  sim_elapsed_seconds: sequence * 60,
  outdoor_temperature_c: -5,
  supply_temperature_c: 35,
  room_target_c: 21,
  supply_setpoint_c: 35,
  electrical_power_w: 1000,
  thermal_power_w: 3000,
  electrical_energy_wh: sequence,
  compressor_state: "running",
  control_mode: "baseline",
  alarm_codes: [],
  applied_command_seq: 0,
  reported_config_version: 1,
});

describe("chart evidence", () => {
  it("sorts observations and inserts gaps for missing sequences, capture outages and invalid sensors", () => {
    const bad: Sample = { ...sample(5, 50), quality_flags: ["sensor_fault"] };
    const points = samplePoints(
      [bad, sample(1), sample(3), sample(4, 40)],
      (s) => s.indoor_temperature_c,
    );
    expect(points).toEqual([
      [60_000, 21],
      [120_000, null],
      [180_000, 21],
      [210_000, null],
      [240_000, 21],
      [300_000, null],
    ]);
    expect(
      samplePoints([sample(1), sample(2)], () => Number.NaN).every(
        (p) => p[1] === null,
      ),
    ).toBe(true);
  });
  it("distinguishes repeated local hours across the Helsinki DST transition", () => {
    expect(timeLabel("2026-10-25T00:00:00Z", "Europe/Helsinki")).toBe(
      "03:00 GMT+3",
    );
    expect(timeLabel("2026-10-25T01:00:00Z", "Europe/Helsinki")).toBe(
      "03:00 GMT+2",
    );
  });
  it("preserves calendar days while bounding trailing windows in elapsed UTC time", () => {
    expect(historyWindow("day", "2026-10-25", "2026-10-25T22:00:00Z")).toEqual({
      day: "2026-10-25",
    });
    expect(historyWindow("week", "2026-10-25", "2026-10-25T22:00:00Z")).toEqual(
      { from: "2026-10-18T22:00:00.000Z", to: "2026-10-25T22:00:00Z" },
    );
  });
});
