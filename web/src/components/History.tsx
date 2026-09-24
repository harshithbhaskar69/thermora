import { useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import TimeChart from "./TimeChart";
import { historyWindow, timeLabel } from "./chart-data";
import Optimization from "./Optimization";

import {
  api,
  unwrap,
  type Building,
  type Device,
  newRequest,
} from "../api/client";

export default function History({
  device,
  building,
}: {
  device: Device;
  building: Building;
}) {
  const zone = building.preferences.timezone;
  const calendarDay = (at: string) =>
    new Intl.DateTimeFormat("en-CA", {
      timeZone: zone,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).format(new Date(at));
  const [run, setRun] = useState(device.run_id);
  const [day, setDay] = useState(
    calendarDay(device.latest?.observed_at ?? "2026-01-15T00:00:00Z"),
  );
  const [before, setBefore] = useState<number>();
  const [period, setPeriod] = useState("day");
  const runs = useQuery({
    queryKey: ["runs", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/runs", {
          params: { path: { device_id: device.id } },
        }),
      ),
  });
  const commands = useQuery({
    queryKey: ["history-commands", device.id, run, before],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/commands", {
          params: {
            path: { device_id: device.id },
            query: { run_id: run, before_sequence: before, limit: 100 },
          },
        }),
      ),
  });
  const cache = useQueryClient();
  const replaySubmission = useRef<{
    payload: string;
    headers: ReturnType<typeof newRequest>;
  } | null>(null);
  const selectedRun = runs.data?.find((r) => r.run_id === run);
  const window = historyWindow(
    period,
    day,
    selectedRun?.last_observed_at ??
      (run === device.run_id ? device.latest?.observed_at : undefined),
  );
  const history = useQuery({
    queryKey: ["history", device.id, run, window],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/history", {
          params: {
            path: { device_id: device.id },
            query: { run_id: run, ...window, bucket_minutes: 60 },
          },
        }),
      ),
    enabled: !!day && (period === "day" || !!window.to),
    refetchInterval: 10_000,
  });

  const replays = useQuery({
    queryKey: ["replays", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/replays", {
          params: { path: { device_id: device.id } },
        }),
      ),
  });
  const replay = useMutation({
    mutationFn: async () => {
      if (!selectedRun?.first_observed_at || !selectedRun.last_observed_at)
        throw new Error("Wait for recorded observations.");
      const end = selectedRun.last_observed_at;
      const start = new Date(
        Math.max(
          Date.parse(selectedRun.first_observed_at),
          Date.parse(end) - 24 * 3600 * 1000,
        ),
      ).toISOString();
      const body = { run_id: run, from: start, to: end };
      const payload = JSON.stringify(body);
      if (replaySubmission.current?.payload !== payload)
        replaySubmission.current = { payload, headers: newRequest() };
      return unwrap(
        await api.POST("/api/v1/devices/{device_id}/replays", {
          params: {
            path: { device_id: device.id },
            header: replaySubmission.current.headers,
          },
          body,
        }),
      );
    },
    onSuccess: () => {
      replaySubmission.current = null;
      void cache.invalidateQueries({ queryKey: ["replays", device.id] });
    },
  });
  const h = history.data;
  const tick = (at: string) => timeLabel(at, zone, true);
  return (
    <>
      <section className="panel">
        <h2>History & statistics</h2>
        <p className="muted">Recorded operation · observed in simulation</p>
        <div className="history-filters">
          <label>
            Window
            <select value={period} onChange={(e) => setPeriod(e.target.value)}>
              <option value="day">Calendar day</option>
              <option value="24h">Last recorded 24 hours</option>
              <option value="week">Last recorded 7 days</option>
            </select>
          </label>
          <label>
            Simulation run
            <select
              value={run}
              onChange={(e) => {
                setRun(e.target.value);
                setBefore(undefined);
                const r = runs.data?.find((r) => r.run_id === e.target.value);
                if (r?.last_observed_at)
                  setDay(calendarDay(r.last_observed_at));
              }}
            >
              {!runs.data?.length && (
                <option value={device.run_id}>Current run</option>
              )}
              {runs.data?.map((r) => (
                <option key={r.run_id} value={r.run_id}>
                  Run {r.epoch} · {r.model}
                  {r.run_id === device.run_id ? " · current" : ""}
                </option>
              ))}
            </select>
          </label>
          <label>
            Calendar day · {zone}
            <input
              type="date"
              disabled={period !== "day"}
              value={day}
              onChange={(e) => setDay(e.target.value)}
              required
            />
          </label>
        </div>
        {(history.error || runs.error || commands.error) && (
          <p role="alert">
            {(history.error || runs.error || commands.error)?.message}
          </p>
        )}
        {history.isPending && (
          <p role="status">Loading historical observations…</p>
        )}
        {h && (
          <>
            <dl className="metrics">
              <div>
                <dt>Covered energy</dt>
                <dd>{h.covered_energy_kwh.toFixed(3)} kWh</dd>
              </div>
              <div>
                <dt>Known cost</dt>
                <dd>
                  {h.priced_seconds > 0
                    ? `€${h.known_cost_eur.toFixed(3)}`
                    : "—"}
                </dd>
              </div>
              <div>
                <dt>Telemetry coverage</dt>
                <dd>
                  {(
                    ((100 * h.covered_seconds) /
                      (Date.parse(h.end) - Date.parse(h.start))) *
                    1000
                  ).toFixed(1)}
                  %
                </dd>
              </div>
            </dl>
            <p className="muted">
              Totals include covered intervals only.{" "}
              {Math.round(h.gap_seconds / 60)} simulated minutes lack usable
              telemetry;{" "}
              {Math.round((h.covered_seconds - h.priced_seconds) / 60)} covered
              minutes lack historical prices or settings.{" "}
              {h.cost_eur === null
                ? "A complete window cost is unavailable."
                : "The full window is priced."}
            </p>
            {h.covered_seconds === 0 ? (
              <p>
                No usable observations in this window. Choose a recorded
                simulation day.
              </p>
            ) : (
              <TimeChart
                timezone={zone}
                label="Historical hourly energy, cost, indoor temperature and setpoints. Missing observations remain gaps."
                rows={[
                  {
                    unit: "Energy · kWh",
                    zero: true,
                    series: [
                      {
                        name: "Covered energy",
                        color: "power",
                        bars: true,
                        values: h.buckets.map((b) => [
                          Date.parse(b.start),
                          b.covered_seconds > 0 ? b.covered_energy_kwh : null,
                        ]),
                      },
                    ],
                  },
                  {
                    unit: "Cost · EUR",
                    zero: true,
                    series: [
                      {
                        name: "Known cost",
                        color: "indoor",
                        bars: true,
                        values: h.buckets.map((b) => [
                          Date.parse(b.start),
                          b.priced_seconds > 0 ? b.known_cost_eur : null,
                        ]),
                      },
                    ],
                  },
                  {
                    unit: "Temperature · °C",
                    series: [
                      {
                        name: "Mean indoor",
                        color: "indoor",
                        values: h.buckets.map((b) => [
                          Date.parse(b.start),
                          b.indoor_mean_c ?? null,
                        ]),
                      },
                      {
                        name: "Last room target",
                        color: "target",
                        step: true,
                        values: h.buckets.map((b) => [
                          Date.parse(b.start),
                          b.last_room_target_c ?? null,
                        ]),
                      },
                      {
                        name: "Last supply target",
                        color: "outdoor",
                        step: true,
                        values: h.buckets.map((b) => [
                          Date.parse(b.start),
                          b.last_supply_setpoint_c ?? null,
                        ]),
                      },
                    ],
                  },
                ]}
              />
            )}
            <details>
              <summary>Hourly values and coverage</summary>
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>Time</th>
                      <th>Energy kWh</th>
                      <th>Known cost €</th>
                      <th>Covered min</th>
                      <th>Supply target °C</th>
                    </tr>
                  </thead>
                  <tbody>
                    {h.buckets.map((b) => (
                      <tr key={b.start}>
                        <td>{tick(b.start)}</td>
                        <td>
                          {b.covered_seconds > 0
                            ? b.covered_energy_kwh.toFixed(3)
                            : "—"}
                        </td>
                        <td>
                          {b.priced_seconds > 0
                            ? b.known_cost_eur.toFixed(3)
                            : "—"}
                        </td>
                        <td>{Math.round(b.covered_seconds / 60)}</td>
                        <td>{b.last_supply_setpoint_c?.toFixed(1) ?? "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </details>
          </>
        )}
        <details>
          <summary>Compare recorded policy with a baseline</summary>
          <p>
            Replay the latest recorded 24 hours of the selected run, followed by
            six recovery hours. Manual interventions apply to both model
            trajectories. Gaps, unknown actions, endpoint mismatch or unequal
            final temperatures prevent a savings claim.
          </p>
          <button
            type="button"
            disabled={replay.isPending || !selectedRun?.last_observed_at}
            onClick={() => replay.mutate()}
          >
            {replay.isPending
              ? "Evaluating recorded policy…"
              : "Compare recorded policy"}
          </button>
          {(replay.error || replays.error) && (
            <p role="alert">{(replay.error || replays.error)?.message}</p>
          )}
          {replays.data
            ?.filter((r) => r.run_id === run)
            .map((r) => (
              <article key={r.id} className="report">
                <h3>Observed-policy model replay</h3>
                <p>
                  {timeLabel(r.start, zone, true)} –{" "}
                  {timeLabel(r.end, zone, true)} ·{" "}
                  {r.reason.replaceAll("_", " ")}
                </p>
                <p>
                  Observed energy: {r.observed_energy_kwh.toFixed(3)} kWh.
                  Observed cost:{" "}
                  {r.observed_cost_eur === null
                    ? "incomplete"
                    : `€${r.observed_cost_eur.toFixed(3)}`}
                  .
                </p>
                {r.baseline && r.replayed && (
                  <p>
                    Model energy including recovery: baseline{" "}
                    {r.baseline.energy_kwh.toFixed(3)} kWh; recorded policy{" "}
                    {r.replayed.energy_kwh.toFixed(3)} kWh.
                  </p>
                )}
                <p>
                  {r.comparable && r.savings_eur !== null
                    ? `Model cost difference including recovery: €${r.savings_eur.toFixed(3)}. Negative means the recorded policy cost more.`
                    : "A savings comparison is unavailable for this evidence."}
                </p>
                <small>
                  Sampled-policy approximation · inputs{" "}
                  {r.input_hash.slice(0, 12)}
                </small>
              </article>
            ))}
        </details>
        <details>
          <summary>Command history for this run</summary>
          <p>
            Newest 100 commands per page. Status distinguishes delivery,
            application and telemetry confirmation.
          </p>
          {!commands.data?.length ? (
            <p>No commands in this page.</p>
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Sequence</th>
                    <th>Issued · wall time</th>
                    <th>Reason</th>
                    <th>Status</th>
                    <th>Room target °C</th>
                  </tr>
                </thead>
                <tbody>
                  {commands.data.map((c) => (
                    <tr key={c.id}>
                      <td>{c.sequence}</td>
                      <td>{new Date(c.issued_at_wall).toLocaleString()}</td>
                      <td>{c.reason_code.replaceAll("_", " ")}</td>
                      <td>{c.stage}</td>
                      <td>{c.room_target_c}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <button
            disabled={before === undefined}
            onClick={() => setBefore(undefined)}
          >
            Newest commands
          </button>
          <button
            disabled={commands.data?.length !== 100}
            onClick={() => setBefore(commands.data?.at(-1)?.sequence)}
          >
            Older commands
          </button>
        </details>
      </section>
      <Optimization device={device} building={building} view="comparisons" />
    </>
  );
}
