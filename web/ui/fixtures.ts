import type { Page } from "@playwright/test";
import { readFile } from "node:fs/promises";
import type { Building, Command, Device, Sample } from "../src/api/client";
import type { components } from "../src/api/schema.gen";

export const buildingID = "11111111-1111-4111-8111-111111111111";
export const deviceID = "22222222-2222-4222-8222-222222222222";
export const secondID = "33333333-3333-4333-8333-333333333333";
export const runID = "44444444-4444-4444-8444-444444444444";
export const orgID = "55555555-5555-4555-8555-555555555555";

// Clearly synthetic, deterministic browser fixtures. Never shipped with the app.
export function samples(): Sample[] {
  return Array.from({ length: 221 }, (_, i) => ({
    sample_seq: i + 1,
    sim_elapsed_seconds: i * 60,
    observed_at: new Date(Date.UTC(2026, 0, 15, 7, i)).toISOString(),
    recorded_at_wall: new Date(Date.UTC(2026, 8, 26, 10, 0, i)).toISOString(),
    indoor_temperature_c: 21.1 + Math.sin(i / 12) * 0.18,
    outdoor_temperature_c: -5,
    supply_temperature_c: 34.8,
    room_target_c: i % 90 > 40 ? 21.5 : 21,
    supply_setpoint_c: 35,
    electrical_power_w: i % 60 < 45 ? 1840 : 20,
    thermal_power_w: i % 60 < 45 ? 5500 : 0,
    electrical_energy_wh: 12406 + i * 15,
    compressor_state: i % 60 < 45 ? "running" : "off",
    control_mode: "remote",
    alarm_codes: [],
    applied_command_seq: 10,
    reported_config_version: 1,
    quality_flags: [],
  }));
}

const intervalStart = (i: number) =>
  new Date(Date.UTC(2026, 9, 25, 0, i * 15)).toISOString();
const outcome: components["schemas"]["ModelOutcome"] = {
  energy_kwh: 12,
  cost_eur: 1.8,
  comfort_degree_hours: 0,
  terminal_indoor_c: 21,
  terminal_supply_c: 35,
  terminal_compressor_on: false,
  terminal_dwell_remaining_sim_seconds: 0,
};
export const plan: components["schemas"]["OptimizationPlan"] = {
  scenario: "negative-price",
  tariff: "variable",
  input_hash: "browser-fixture",
  model_version: "fixture",
  controller_version: "fixture",
  provenance: "modelled_comparison",
  start: intervalStart(0),
  end: intervalStart(96),
  recovery_end: intervalStart(120),
  initial_indoor_c: 21,
  initial_supply_c: 35,
  comfort: { target_c: 21, min_c: 20, max_c: 22 },
  adopted: false,
  reason_code: "fallback_constraints_or_cost",
  comparable: true,
  savings_eur: 0,
  baseline: outcome,
  optimized: outcome,
  intervals: Array.from({ length: 96 }, (_, i) => ({
    start: intervalStart(i),
    end: intervalStart(i + 1),
    price_eur_per_kwh: i === 4 ? null : i < 12 ? -0.05 : 0.18,
    forecast_outdoor_c: i === 4 ? null : -5,
    room_target_c: 21,
    reason_code: "normal",
  })),
  trace: Array.from({ length: 120 }, (_, i) => ({
    at: intervalStart(i),
    baseline_indoor_c: 21 + Math.sin(i / 6) * 0.2,
    optimized_indoor_c: 21 + Math.sin(i / 6) * 0.2,
    baseline_energy_kwh: (i + 1) / 10,
    optimized_energy_kwh: (i + 1) / 10,
  })),
};
const history: components["schemas"]["History"] = {
  run_id: runID,
  start: intervalStart(0),
  end: intervalStart(16),
  timezone: "Europe/Helsinki",
  provenance: "observed_in_simulation",
  covered_seconds: 10800,
  priced_seconds: 7200,
  gap_seconds: 3600,
  covered_energy_kwh: 3,
  known_cost_eur: 0.13,
  cost_eur: null,
  comfort_degree_hours: 0,
  buckets: Array.from({ length: 4 }, (_, i) => ({
    start: intervalStart(i * 4),
    end: intervalStart(i * 4 + 4),
    covered_seconds: i === 2 ? 0 : 3600,
    priced_seconds: i === 0 || i === 3 ? 3600 : 0,
    covered_energy_kwh: i === 2 ? 0 : 1,
    known_cost_eur: i === 0 ? -0.05 : i === 3 ? 0.18 : 0,
    cost_eur: i === 0 ? -0.05 : i === 3 ? 0.18 : null,
    indoor_mean_c: i === 2 ? null : 21,
    last_room_target_c: i === 2 ? null : 21,
    last_supply_setpoint_c: i === 2 ? null : 35,
    comfort_degree_hours: 0,
  })),
};
export async function installAPI(page: Page) {
  await page.route("**/openapi.yaml", async (route) =>
    route.fulfill({
      contentType: "application/yaml",
      body: await readFile(
        new URL("../../api/openapi.yaml", import.meta.url),
        "utf8",
      ),
    }),
  );
  const series = samples();
  const device: Device = {
    id: deviceID,
    building_id: buildingID,
    run_id: runID,
    name: "Main heat pump",
    enrollment: "active",
    connectivity: "online",
    desired_version: 1,
    reported_version: 1,
    generation: 1,
    origin: "simulation",
    control_connected: true,
    last_received_at: "2026-09-26T10:03:40Z",
    latest: series.at(-1),
  };
  const building: Building = {
    id: buildingID,
    organization_id: orgID,
    name: "Lakeside home",
    version: 1,
    preferences: {
      timezone: "Europe/Helsinki",
      comfort_min_c: 20,
      comfort_max_c: 22,
      comfort_target_c: 21,
    },
    devices: [device, { ...device, id: secondID, name: "Garden studio" }],
  };
  const commands: Command[] = Array.from({ length: 3 }, (_, i) => ({
    id: `command-${i}`,
    sequence: 10 - i,
    stage: "confirmed",
    reason_code: i === 1 ? "manual_override" : "price_schedule",
    room_target_c: 21,
    supply_setpoint_c: 35,
    mode: "heat",
    issued_at_wall: `2026-09-26T10:${35 - i * 10}:00Z`,
    expires_at_wall: "2026-09-26T11:00:00Z",
    valid_until: "2026-01-15T11:00:00Z",
    events: ["published", "accepted", "applied", "confirmed"].map((stage) => ({
      stage,
      occurred_at_wall: "2026-09-26T10:35:01Z",
      reason_code: "",
    })),
  }));
  const state = {
    building,
    series,
    commands,
    requests: [] as string[],
    denied: false,
    overrideRejected: false,
    signedIn: true,
    connected: true,
    hasPlan: true,
    profileConflict: false,
    run: {
      run_id: runID,
      epoch: 1,
      rate: 60,
      paused: false,
      model: "heating",
      status: "ready",
    } as components["schemas"]["RunState"],
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    state.requests.push(
      `${route.request().method()} ${url.pathname}${url.search}`,
    );
    const respond = (data: unknown, status = 200) =>
      route.fulfill({
        status,
        contentType: "application/json",
        body: JSON.stringify(data),
      });
    if (url.pathname === "/api/v1/session")
      return !state.signedIn
        ? respond({ detail: "Sign in required." }, 401)
        : respond({
            user_id: "ui-owner",
            login: "owner@thermora.local",
            organizations: [
              { id: orgID, name: "Your workspace", role: "operator" },
            ],
          });
    if (state.denied)
      return respond(
        { detail: "You do not have access to this building." },
        403,
      );
    if (url.pathname.endsWith("/buildings")) return respond([building]);
    if (url.pathname.endsWith("/profile")) {
      if (route.request().method() === "PUT" && state.profileConflict)
        return respond({ detail: "Building changed in another session." }, 412);
      return respond({
        floor_area_m2: 120,
        country_code: "FI",
        address: "Fictional street 1",
        postal_code: "00000",
        city: "Demo city",
        tariff_type: "variable",
      });
    }
    if (url.pathname === `/api/v1/buildings/${buildingID}`)
      return respond(building);
    if (url.pathname.endsWith("/series")) return respond(state.series);
    if (url.pathname.endsWith("/commands")) return respond(state.commands);
    if (url.pathname.endsWith("/overrides"))
      return state.overrideRejected
        ? respond(
            { detail: "Telemetry is stale. Wait for a fresh connection." },
            409,
          )
        : respond({ command_id: "pending-command" }, 202);
    if (url.pathname.endsWith("/run")) return respond(state.run);
    if (url.pathname.endsWith("/runs"))
      return respond([
        {
          run_id: runID,
          epoch: 1,
          model: "heating",
          first_observed_at: intervalStart(0),
          last_observed_at: intervalStart(16),
        },
      ]);
    if (url.pathname.endsWith("/history")) return respond(history);
    if (url.pathname.endsWith("/settings"))
      return respond({
        version: 1,
        model: "heating",
        preferences: { preset: "balanced", flexibility_opt_in: true },
      });
    if (url.pathname.endsWith("/optimization"))
      return respond({
        enabled: state.hasPlan,
        scenario: plan.scenario,
        tariff: "variable",
        integration_route: "manufacturer",
        manufacturer_status: "connected",
        artifacts_enabled: false,
        plan: state.hasPlan ? plan : undefined,
      });
    if (url.pathname.endsWith("/comparisons"))
      return respond([
        {
          id: "comparison-fixture",
          device_id: deviceID,
          created_at_wall: "2026-09-26T10:00:00Z",
          plan,
        },
      ]);
    if (
      url.pathname.endsWith("/replays") ||
      url.pathname.endsWith("/flexibility-events")
    )
      return respond([]);
    if (url.pathname.endsWith("/manufacturers"))
      return respond([
        {
          id: "demo",
          name: "Thermora demo manufacturer",
          prerequisites:
            "Use the separately provisioned local manufacturer account.",
        },
      ]);
    if (url.pathname.endsWith("/manufacturer-connections"))
      return respond(
        state.connected
          ? [
              {
                id: "connection-fixture",
                manufacturer_id: "demo",
                status: "connected",
              },
            ]
          : [],
      );
    if (url.pathname.endsWith("/connection-fixture/devices"))
      return respond([
        { id: deviceID, name: "Main heat pump", available: true },
      ]);
    if (url.pathname.endsWith("/cloud-enrollments"))
      return respond({ detail: "The partner access code is invalid." }, 422);
    if (url.pathname.endsWith("/fleet"))
      return respond(
        building.devices.map((d, i) => ({
          device: d,
          building_name: building.name,
          eligibility: i === 0 ? "eligible" : "owner_opt_out",
        })),
      );
    if (url.pathname.endsWith("/metrics"))
      return respond({
        fresh_devices: 2,
        active_devices: 2,
        pending_deliveries: 0,
        oldest_delivery_seconds: 0,
        mean_application_seconds: 1.2,
        turnaround_samples: 8,
        manufacturer_failures: 0,
        device_diagnostics: [],
      });
    return respond({ detail: "No fixture for this request." }, 404);
  });
  return state;
}
