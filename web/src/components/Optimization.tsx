import { useRef, useState } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import TimeChart from "./TimeChart";
import { timeLabel } from "./chart-data";

import {
  api,
  newRequest,
  unwrap,
  type Building,
  type Device,
} from "../api/client";
import type { components } from "../api/schema.gen";

type Plan = components["schemas"]["OptimizationPlan"];
type Choice =
  "winter" | "negative-price" | "missing-price" | "missing-forecast";
const choices: Choice[] = [
  "winter",
  "negative-price",
  "missing-price",
  "missing-forecast",
];
const label = (value: string) =>
  value.replaceAll("_", " ").replaceAll("-", " ");
function scenario(form: FormData): Choice {
  const value = form.get("scenario");
  return choices.find((choice) => choice === value) ?? "winter";
}
const money = (value: number | null | undefined) =>
  value == null ? "Unavailable" : `€${value.toFixed(3)}`;

export default function Optimization({
  device,
  building,
  view = "schedule",
}: {
  device: Device;
  building: Building;
  view?: "schedule" | "comparisons";
}) {
  const client = useQueryClient();
  const [selectedReport, selectReport] = useState("");
  const [download, setDownload] = useState<{ id: string; url: string }>();
  const settingsKey = useRef({ body: "", header: newRequest() });
  const reportKey = useRef({ body: "", header: newRequest() });
  const state = useQuery({
    queryKey: ["optimization", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/optimization", {
          params: { path: { device_id: device.id } },
        }),
      ),
    refetchInterval: 3000,
  });
  const reports = useQuery({
    queryKey: ["comparisons", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/comparisons", {
          params: { path: { device_id: device.id } },
        }),
      ),
  });
  const settings = useMutation({
    mutationFn: async (body: { enabled: boolean; scenario: Choice }) => {
      const encoded = JSON.stringify(body);
      if (settingsKey.current.body !== encoded)
        settingsKey.current = { body: encoded, header: newRequest() };
      return unwrap(
        await api.PUT("/api/v1/devices/{device_id}/optimization", {
          params: {
            path: { device_id: device.id },
            header: settingsKey.current.header,
          },
          body,
        }),
      );
    },
    onSuccess: () => {
      settingsKey.current.body = "";
      void client.invalidateQueries({ queryKey: ["optimization", device.id] });
      void client.invalidateQueries({ queryKey: ["device", device.id] });
    },
  });
  const compare = useMutation({
    mutationFn: async (body: { scenario: Choice; start: string }) => {
      const encoded = JSON.stringify(body);
      if (reportKey.current.body !== encoded)
        reportKey.current = { body: encoded, header: newRequest() };
      return unwrap(
        await api.POST("/api/v1/devices/{device_id}/comparisons", {
          params: {
            path: { device_id: device.id },
            header: reportKey.current.header,
          },
          body,
        }),
      );
    },
    onSuccess: (report) => {
      reportKey.current.body = "";
      selectReport(report.id);
      void client.invalidateQueries({ queryKey: ["comparisons", device.id] });
    },
  });
  const exportReport = useMutation({
    mutationFn: async (id: string) => ({
      id,
      ...unwrap(
        await api.POST(
          "/api/v1/devices/{device_id}/comparisons/{comparison_id}/artifact",
          { params: { path: { device_id: device.id, comparison_id: id } } },
        ),
      ),
    }),
    onSuccess: (result) =>
      setDownload({ id: result.id, url: result.download_url }),
  });
  const report =
    reports.data?.find((r) => r.id === selectedReport) ?? reports.data?.[0];
  const error =
    state.error ??
    settings.error ??
    reports.error ??
    compare.error ??
    exportReport.error;
  return (
    <section className="panel optimization-panel">
      <h2>
        {view === "schedule"
          ? "Weather & price optimization"
          : "Independent controller comparisons"}
      </h2>
      {state.isPending && <p role="status">Loading controller state…</p>}
      {error && (
        <p className="notice error" role="alert">
          {error.message}
        </p>
      )}
      {view === "schedule" && state.data && (
        <>
          <p className="muted">
            A proposed schedule from packaged weather and electricity prices.
            Equipment confirmation is recorded in Activity.
          </p>
          {state.data.manufacturer_status === "authentication_required" && (
            <p className="notice">
              <Link to="/connect">Reconnect your manufacturer account</Link> to
              grant equipment control.
            </p>
          )}
          <form
            key={`${state.data.enabled}:${state.data.scenario}`}
            onSubmit={(event) => {
              event.preventDefault();
              const form = new FormData(event.currentTarget);
              settings.mutate({
                scenario: scenario(form),
                enabled: form.get("enabled") === "on",
              });
            }}
          >
            <Scenario
              defaultValue={
                state.data.scenario === "constant"
                  ? "winter"
                  : state.data.scenario
              }
            />
            <label>
              <input
                type="checkbox"
                name="enabled"
                defaultChecked={state.data.enabled}
              />{" "}
              Enable price optimization
            </label>
            <p className="footnote">
              {state.data.tariff === "fixed"
                ? "Fixed demo tariff: €0.15/kWh; weather baseline only."
                : "Variable demo tariff: packaged UTC price intervals."}{" "}
              Manual overrides take priority. Missing inputs use the local
              baseline.
            </p>
            <button disabled={settings.isPending}>
              {settings.isPending ? "Saving…" : "Apply scenario & control"}
            </button>
          </form>
          <p role="status">
            Optimization {state.data.enabled ? "enabled" : "disabled"}.
            Configuration{" "}
            {device.reported_version === device.desired_version
              ? "confirmed by equipment"
              : "awaiting equipment confirmation"}
            .
          </p>
          {state.data.enabled && !state.data.plan && (
            <p>
              Waiting for a fresh sample and applied configuration to create a
              schedule.
            </p>
          )}
          {state.data.enabled && state.data.plan && (
            <details open>
              <summary>
                Current proposed schedule · {label(state.data.plan.reason_code)}
              </summary>
              <Schedule
                plan={state.data.plan}
                timezone={building.preferences.timezone}
              />
            </details>
          )}
        </>
      )}
      {view === "comparisons" && (
        <>
          <h3>Compare controllers</h3>
          <p>
            Independent model runs, using identical starting temperatures,
            weather and comfort bounds. Includes 24 hours of control and six
            hours of baseline recovery. This is not a measurement of savings at
            your building.
          </p>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              const form = new FormData(event.currentTarget);
              const date = form.get("date");
              compare.mutate({
                scenario: scenario(form),
                start: `${typeof date === "string" ? date : ""}T00:00:00Z`,
              });
            }}
          >
            <Scenario defaultValue="winter" />
            <label>
              Evaluation date · UTC
              <input
                type="date"
                name="date"
                defaultValue="2026-01-15"
                min="2026-01-15"
                max="2100-01-01"
                required
              />
            </label>
            <button disabled={compare.isPending}>
              {compare.isPending ? "Comparing…" : "Compare controllers"}
            </button>
          </form>
          {reports.isPending && <p role="status">Loading comparisons…</p>}
          {reports.data?.length === 0 && (
            <p>
              No comparisons yet. Select a scenario to evaluate both
              controllers.
            </p>
          )}
          {!!reports.data?.length && (
            <label>
              Saved comparison
              <select
                value={report?.id}
                onChange={(event) => selectReport(event.target.value)}
              >
                {reports.data.map((r) => (
                  <option key={r.id} value={r.id}>
                    {label(r.plan.scenario)} ·{" "}
                    {new Date(r.created_at_wall).toLocaleString()}
                  </option>
                ))}
              </select>
            </label>
          )}
          {report && (
            <>
              <p className="pill subtle">Modelled comparison</p>
              <p>
                {label(report.plan.reason_code)}.{" "}
                {report.plan.comparable
                  ? `Modelled cost difference: ${money(report.plan.savings_eur)}.`
                  : `Comparison unavailable: ${label(report.plan.comparison_reason ?? "terminal_states_differ")}. Raw outcomes are shown below.`}
              </p>
              <TimeChart
                timezone={building.preferences.timezone}
                label="Independent model comparison, including recovery. Baseline and price candidate temperature and cumulative energy."
                rows={[
                  {
                    unit: "Indoor temperature · °C",
                    comfort: [
                      report.plan.comfort.min_c,
                      report.plan.comfort.max_c,
                    ],
                    series: [
                      {
                        name: "Baseline temperature",
                        color: "outdoor",
                        values: report.plan.trace.map((p) => [
                          Date.parse(p.at),
                          p.baseline_indoor_c,
                        ]),
                      },
                      {
                        name: "Candidate temperature",
                        color: "indoor",
                        values: report.plan.trace.map((p) => [
                          Date.parse(p.at),
                          p.optimized_indoor_c,
                        ]),
                      },
                    ],
                  },
                  {
                    unit: "Cumulative energy · kWh",
                    zero: true,
                    series: [
                      {
                        name: "Baseline energy",
                        color: "outdoor",
                        values: report.plan.trace.map((p) => [
                          Date.parse(p.at),
                          p.baseline_energy_kwh,
                        ]),
                      },
                      {
                        name: "Candidate energy",
                        color: "power",
                        values: report.plan.trace.map((p) => [
                          Date.parse(p.at),
                          p.optimized_energy_kwh,
                        ]),
                      },
                    ],
                  },
                ]}
              />
              <details>
                <summary>Model trajectory values</summary>
                <div className="table-scroll">
                  <table>
                    <thead>
                      <tr>
                        <th>Simulated time</th>
                        <th>Baseline °C</th>
                        <th>Candidate °C</th>
                        <th>Baseline kWh</th>
                        <th>Candidate kWh</th>
                      </tr>
                    </thead>
                    <tbody>
                      {report.plan.trace.map((p) => (
                        <tr key={p.at}>
                          <td>
                            {timeLabel(
                              p.at,
                              building.preferences.timezone,
                              true,
                            )}
                          </td>
                          <td>{p.baseline_indoor_c.toFixed(3)}</td>
                          <td>{p.optimized_indoor_c.toFixed(3)}</td>
                          <td>{p.baseline_energy_kwh.toFixed(3)}</td>
                          <td>{p.optimized_energy_kwh.toFixed(3)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </details>
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>30-hour outcome</th>
                      <th>Baseline</th>
                      <th>Price candidate</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr>
                      <th>Electrical energy · kWh</th>
                      <td>{report.plan.baseline.energy_kwh.toFixed(3)}</td>
                      <td>{report.plan.optimized.energy_kwh.toFixed(3)}</td>
                    </tr>
                    <tr>
                      <th>Energy cost · EUR</th>
                      <td>{money(report.plan.baseline.cost_eur)}</td>
                      <td>{money(report.plan.optimized.cost_eur)}</td>
                    </tr>
                    <tr>
                      <th>Comfort violation · °C·h</th>
                      <td>
                        {report.plan.baseline.comfort_degree_hours.toFixed(4)}
                      </td>
                      <td>
                        {report.plan.optimized.comfort_degree_hours.toFixed(4)}
                      </td>
                    </tr>
                    <tr>
                      <th>Terminal indoor · °C</th>
                      <td>
                        {report.plan.baseline.terminal_indoor_c.toFixed(3)}
                      </td>
                      <td>
                        {report.plan.optimized.terminal_indoor_c.toFixed(3)}
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
              <p className="footnote">
                Terminal supply: baseline{" "}
                {report.plan.baseline.terminal_supply_c.toFixed(2)} °C,
                candidate {report.plan.optimized.terminal_supply_c.toFixed(2)}{" "}
                °C. Compressor: baseline{" "}
                {report.plan.baseline.terminal_compressor_on ? "on" : "off"},
                candidate{" "}
                {report.plan.optimized.terminal_compressor_on ? "on" : "off"}.
                Dwell remaining:{" "}
                {report.plan.baseline.terminal_dwell_remaining_sim_seconds} /{" "}
                {report.plan.optimized.terminal_dwell_remaining_sim_seconds}{" "}
                simulated seconds. Starting indoor{" "}
                {report.plan.initial_indoor_c} °C, supply{" "}
                {report.plan.initial_supply_c} °C, compressor off with a
                five-minute dwell. Cost excludes standing charges and taxes. The
                model assumes timely commands.
              </p>
              <details>
                <summary>Comparison schedule, prices and forecast</summary>
                <Schedule
                  plan={report.plan}
                  timezone={building.preferences.timezone}
                />
              </details>
              {state.data?.artifacts_enabled && (
                <>
                  <button
                    type="button"
                    disabled={exportReport.isPending}
                    onClick={() => exportReport.mutate(report.id)}
                  >
                    {exportReport.isPending
                      ? "Saving report…"
                      : "Save report to local S3"}
                  </button>
                  {download?.id === report.id && (
                    <p>
                      <a href={download.url}>Download comparison JSON</a>
                    </p>
                  )}
                </>
              )}
            </>
          )}
        </>
      )}
    </section>
  );
}

function Scenario({ defaultValue }: { defaultValue: string }) {
  return (
    <label>
      Scenario
      <select name="scenario" defaultValue={defaultValue}>
        {choices.map((choice) => (
          <option key={choice} value={choice}>
            {label(choice)}
          </option>
        ))}
      </select>
    </label>
  );
}

function Schedule({ plan, timezone }: { plan: Plan; timezone: string }) {
  const time = (value: string) =>
    new Intl.DateTimeFormat("en-GB", {
      timeZone: timezone,
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      timeZoneName: "shortOffset",
    }).format(new Date(value));
  return (
    <>
      <p className="footnote">
        Forecast · {timezone}. Negative prices are retained; gaps indicate
        missing inputs. Targets are planned, not proof of execution.
      </p>
      <TimeChart
        timezone={timezone}
        label="Forecast prices, outdoor temperature and planned room targets over simulated time."
        rows={[
          {
            unit: "Price · EUR/kWh",
            zero: true,
            series: [
              {
                name: "Forecast price",
                color: "power",
                bars: true,
                values: plan.intervals.map((i) => [
                  Date.parse(i.start),
                  i.price_eur_per_kwh ?? null,
                ]),
              },
            ],
          },
          {
            unit: "Temperature · °C",
            series: [
              {
                name: "Forecast outdoor",
                color: "outdoor",
                values: plan.intervals.map((i) => [
                  Date.parse(i.start),
                  i.forecast_outdoor_c ?? null,
                ]),
              },
              {
                name: "Planned room target",
                color: "target",
                step: true,
                values: plan.intervals.map((i) => [
                  Date.parse(i.start),
                  i.room_target_c,
                ]),
              },
            ],
          },
        ]}
      />
      <details>
        <summary>All {plan.intervals.length} schedule intervals</summary>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Start (15-minute interval)</th>
                <th>EUR/kWh</th>
                <th>Outdoor °C</th>
                <th>Room °C</th>
                <th>Reason</th>
              </tr>
            </thead>
            <tbody>
              {plan.intervals.map((interval) => (
                <tr key={interval.start}>
                  <td>{time(interval.start)}</td>
                  <td>{interval.price_eur_per_kwh ?? "Missing"}</td>
                  <td>
                    {interval.forecast_outdoor_c?.toFixed(1) ?? "Missing"}
                  </td>
                  <td>{interval.room_target_c}</td>
                  <td>{label(interval.reason_code)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </>
  );
}
