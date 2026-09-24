import { useRef, useState } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, newRequest, unwrap, type Session } from "./api/client";

export default function Fleet({ session }: { session: Session }) {
  const organizations = session.organizations.filter(
    (o) => o.role === "operator",
  );
  const [org, setOrg] = useState(organizations[0]?.id ?? "");
  const [duration, setDuration] = useState(15);
  const [event, setEvent] = useState("");
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const key = useRef(newRequest());
  const cache = useQueryClient();
  const fleet = useQuery({
    queryKey: ["fleet", org],
    enabled: !!org,
    refetchInterval: 5000,
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/organizations/{organization_id}/fleet", {
          params: { path: { organization_id: org } },
        }),
      ),
  });
  const events = useQuery({
    queryKey: ["flex-events", org],
    enabled: !!org,
    refetchInterval: 5000,
    queryFn: async () =>
      unwrap(
        await api.GET(
          "/api/v1/organizations/{organization_id}/flexibility-events",
          { params: { path: { organization_id: org } } },
        ),
      ),
  });
  const selected = event || events.data?.[0]?.id || "";
  const evidence = useQuery({
    queryKey: ["flex-event", org, selected],
    enabled: !!selected,
    refetchInterval: 5000,
    queryFn: async () =>
      unwrap(
        await api.GET(
          "/api/v1/organizations/{organization_id}/flexibility-events/{event_id}",
          { params: { path: { organization_id: org, event_id: selected } } },
        ),
      ),
  });
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST(
          "/api/v1/organizations/{organization_id}/flexibility-events",
          {
            params: { path: { organization_id: org }, header: key.current },
            body: { duration_sim_minutes: duration },
          },
        ),
      ),
    onSuccess: (value) => {
      key.current = newRequest();
      setEvent(value.id);
      void cache.invalidateQueries({ queryKey: ["flex-events", org] });
    },
  });
  const cancel = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.DELETE(
          "/api/v1/organizations/{organization_id}/flexibility-events/{event_id}",
          { params: { path: { organization_id: org, event_id: selected } } },
        ),
      ),
    onSuccess: () => {
      void cache.invalidateQueries({ queryKey: ["flex-events", org] });
      void cache.invalidateQueries({ queryKey: ["flex-event", org, selected] });
    },
  });
  const metrics = useQuery({
    queryKey: ["metrics", org],
    enabled: !!org,
    refetchInterval: 5000,
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/organizations/{organization_id}/metrics", {
          params: { path: { organization_id: org } },
        }),
      ),
  });
  const visibleDevices = fleet.data?.filter((entry) => {
    const matches = `${entry.building_name} ${entry.device.name}`
      .toLocaleLowerCase()
      .includes(search.toLocaleLowerCase());
    return (
      matches &&
      (filter === "all" ||
        (filter === "eligible"
          ? entry.eligibility === "eligible"
          : entry.eligibility !== "eligible"))
    );
  });
  const error =
    metrics.error ||
    fleet.error ||
    events.error ||
    evidence.error ||
    create.error ||
    cancel.error;
  if (!organizations.length)
    return (
      <section className="panel">
        <h1>Operator access required</h1>
        <p>This account has no organization operator role.</p>
        <Link to="/">Back to buildings</Link>
      </section>
    );
  return (
    <>
      <div className="page-heading">
        <div>
          <h1>Fleet operations</h1>
          <p className="muted">
            Availability, owner consent and simulated dispatch.
          </p>
        </div>
        <span className="pill">Organization operator</span>
      </div>
      {organizations.length > 1 && (
        <label>
          Organization
          <select
            value={org}
            onChange={(e) => {
              setOrg(e.target.value);
              setEvent("");
            }}
          >
            {organizations.map((o) => (
              <option key={o.id} value={o.id}>
                {o.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {error && (
        <p className="notice error" role="alert">
          {error.message}
        </p>
      )}
      {metrics.data && (
        <section className="panel">
          <h2>Operational health</h2>
          <div className="metrics">
            <div>
              <span>Fresh devices</span>
              <strong>
                {metrics.data.fresh_devices} / {metrics.data.active_devices}
              </strong>
            </div>
            <div>
              <span>Pending deliveries</span>
              <strong>{metrics.data.pending_deliveries}</strong>
              <small>
                Oldest {metrics.data.oldest_delivery_seconds.toFixed(1)} s
              </small>
            </div>
            <div>
              <span>Mean command application</span>
              <strong>
                {metrics.data.mean_application_seconds?.toFixed(2) ?? "—"} s
              </strong>
              <small>
                {metrics.data.turnaround_samples} samples · last 24 wall hours
              </small>
            </div>
            <div>
              <span>Manufacturer poll failures</span>
              <strong>{metrics.data.manufacturer_failures}</strong>
            </div>
          </div>
          <details>
            <summary>Edge buffers and freshness</summary>
            <ul>
              {metrics.data.device_diagnostics.map((d) => (
                <li key={d.device_id}>
                  {fleet.data?.find((f) => f.device.id === d.device_id)?.device
                    .name ?? d.device_id.slice(0, 8)}
                  : telemetry {d.telemetry_age_seconds?.toFixed(1) ?? "unknown"}{" "}
                  s old;{" "}
                  {d.edge
                    ? `${d.edge.buffered_batches} batches, ${(d.edge.buffered_bytes / 1024).toFixed(1)} KiB occupied, ${d.edge.pending_events} pending events`
                    : "edge diagnostics unavailable"}
                  {d.edge &&
                    ` · diagnostic captured ${new Date(d.edge.recorded_at_wall).toLocaleTimeString()}`}
                  {d.credential_revocation_pending &&
                    " · credential revocation pending"}
                </li>
              ))}
            </ul>
          </details>
        </section>
      )}
      <section className="panel">
        <h2>Equipment availability</h2>
        <div className="fleet-filters">
          <label>
            Find building or zone
            <input
              type="search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search equipment"
            />
          </label>
          <label>
            Availability
            <select value={filter} onChange={(e) => setFilter(e.target.value)}>
              <option value="all">All equipment</option>
              <option value="eligible">Eligible for dispatch</option>
              <option value="excluded">Needs attention / excluded</option>
            </select>
          </label>
        </div>
        {fleet.data?.length !== 0 && visibleDevices?.length === 0 && (
          <p role="status">No equipment matches these filters.</p>
        )}
        {fleet.isPending ? (
          <p role="status">Loading fleet…</p>
        ) : !fleet.data?.length ? (
          <p>No enrolled equipment in this organization.</p>
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Building / zone</th>
                  <th>Connection</th>
                  <th>Indoor</th>
                  <th>Power</th>
                  <th>Dispatch eligibility</th>
                </tr>
              </thead>
              <tbody>
                {visibleDevices?.map(
                  ({ device: d, building_name, eligibility }) => (
                    <tr key={d.id}>
                      <td>
                        <Link to={`/buildings/${d.building_id}?device=${d.id}`}>
                          {building_name}
                        </Link>
                        <br />
                        {d.name}
                      </td>
                      <td>{d.connectivity}</td>
                      <td>
                        {d.latest?.indoor_temperature_c.toFixed(1) ?? "—"} °C
                      </td>
                      <td>
                        {d.latest
                          ? (d.latest.electrical_power_w / 1000).toFixed(2)
                          : "—"}{" "}
                        kW
                      </td>
                      <td>{eligibility.replaceAll("_", " ")}</td>
                    </tr>
                  ),
                )}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <section className="panel">
        <h2>Simulated flexibility event</h2>
        <p>
          Eligible zones temporarily use their comfort minimum + 0.25 °C. Owner
          overrides, hot-water priority and equipment protections take
          precedence. An excluded device cannot rejoin the same event.
        </p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <div className="form-row">
            <label>
              Duration · simulated minutes
              <input
                type="number"
                min={5}
                max={60}
                value={duration}
                onChange={(e) => {
                  setDuration(e.target.valueAsNumber);
                  key.current = newRequest();
                }}
                required
              />
            </label>
            <button
              disabled={
                create.isPending ||
                events.data?.some((e) => e.status === "active") ||
                !fleet.data?.some((d) => d.eligibility === "eligible")
              }
            >
              {create.isPending ? "Creating event…" : "Start simulated event"}
            </button>
          </div>
        </form>
        {!!events.data?.length && (
          <label>
            Event history
            <select value={selected} onChange={(e) => setEvent(e.target.value)}>
              {events.data?.map((e) => (
                <option key={e.id} value={e.id}>
                  {new Date(e.created_at_wall).toLocaleString()} · {e.status}
                </option>
              ))}
            </select>
          </label>
        )}
        {evidence.data && (
          <>
            <p role="status">
              Event {evidence.data.status} ·{" "}
              {evidence.data.duration_sim_minutes} simulated minutes per zone.
            </p>
            {evidence.data.status === "active" && (
              <button
                className="secondary"
                disabled={cancel.isPending}
                onClick={() => cancel.mutate()}
              >
                Cancel event
              </button>
            )}
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Zone</th>
                    <th>Outcome</th>
                    <th>Command states</th>
                    <th>Covered minutes</th>
                    <th>Energy kWh</th>
                    <th>Before / during kW</th>
                  </tr>
                </thead>
                <tbody>
                  {evidence.data.participants.map((p) => (
                    <tr key={p.device_id}>
                      <td>
                        {fleet.data?.find((d) => d.device.id === p.device_id)
                          ?.device.name ?? p.device_id.slice(0, 8)}
                      </td>
                      <td>
                        {p.status} · {p.reason.replaceAll("_", " ")}
                      </td>
                      <td>
                        {p.command_counts
                          .map((c) => `${c.count} ${c.stage}`)
                          .join(" · ") || "No command issued"}
                      </td>
                      <td>{(p.covered_seconds / 60).toFixed(1)}</td>
                      <td>{p.covered_energy_kwh?.toFixed(3) ?? "—"}</td>
                      <td>
                        {p.reference_power_kw?.toFixed(2) ?? "—"} /{" "}
                        {p.observed_mean_kw?.toFixed(2) ?? "incomplete"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="muted">
              Before is one pre-event reading; during is a counter-based mean
              only when the full interval has usable telemetry. This
              before/after comparison does not establish causal savings or
              verified demand response. Each independent zone has its own
              simulation clock.
            </p>
          </>
        )}
      </section>
    </>
  );
}
