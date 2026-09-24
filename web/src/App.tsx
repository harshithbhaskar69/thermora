import { ThemeToggle } from "./components/Theme";
import { DeviceSettings } from "./components/DeviceSettings";
import { RunControls, useRun } from "./components/RunControls";
import {
  BuildingSettings,
  DeviceConnection,
} from "./components/BuildingSettings";
import { lazy, Suspense, useState } from "react";
import {
  Link,
  NavLink,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  api,
  APIError,
  newRequest,
  unwrap,
  type Building,
  type Command,
  type Device,
  type Session,
} from "./api/client";

const TelemetryCharts = lazy(() => import("./components/TelemetryCharts"));
const Fleet = lazy(() => import("./Fleet"));
const Onboarding = lazy(() => import("./Onboarding"));
const History = lazy(() => import("./components/History"));
const Optimization = lazy(() => import("./components/Optimization"));

function Mark() {
  return (
    <svg
      aria-hidden="true"
      width="32"
      height="32"
      viewBox="0 0 32 32"
      fill="none"
    >
      <path
        d="M3 11c5-9 12 9 18 0 3-4 5-4 8-2M3 22c5-9 12 9 18 0 3-4 5-4 8-2"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
    </svg>
  );
}
function Notice({ error }: { error: unknown }) {
  return error ? (
    <p className="notice error" role="alert">
      {error instanceof Error ? error.message : "Something went wrong."}
    </p>
  ) : null;
}
function Loading() {
  return (
    <p className="notice" role="status">
      Loading workspace…
    </p>
  );
}

export function App() {
  const [menuOpen, setMenuOpen] = useState(false);
  const { pathname } = useLocation();
  const client = useQueryClient();
  const session = useQuery({
    queryKey: ["session"],
    queryFn: async () => unwrap(await api.GET("/api/v1/session")),
  });
  const logout = useMutation({
    mutationFn: async () => unwrap(await api.DELETE("/api/v1/session")),
    onSuccess: () => {
      client.clear();
      void client.invalidateQueries();
    },
  });
  if (session.isPending)
    return (
      <main className="login">
        <Loading />
      </main>
    );
  if (!session.data) {
    if (session.error instanceof APIError && session.error.status === 401)
      return <Login />;
    return (
      <main className="login">
        <h1>Thermora is reconnecting</h1>
        <Notice error={session.error} />
        <button onClick={() => void session.refetch()}>Try again</button>
      </main>
    );
  }
  return (
    <div className="app-shell">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <aside className="sidebar">
        <Link className="brand" to="/">
          <Mark />
          Thermora
        </Link>
        <button
          className="secondary mobile-nav-toggle"
          aria-label="Workspace menu"
          aria-controls="workspace-nav"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen(!menuOpen)}
        >
          <NavIcon name="menu" />
        </button>
        <nav
          id="workspace-nav"
          aria-label="Workspace"
          className={menuOpen ? "workspace-nav is-open" : "workspace-nav"}
        >
          <Link
            className="nav-item"
            to="/"
            aria-current={
              pathname === "/"
                ? "page"
                : pathname.startsWith("/buildings/")
                  ? "location"
                  : undefined
            }
            onClick={() => setMenuOpen(false)}
          >
            <NavIcon name="building" />
            Buildings
          </Link>
          <NavLink
            className="nav-item"
            to="/connect"
            onClick={() => setMenuOpen(false)}
          >
            <NavIcon name="link" />
            Connect equipment
          </NavLink>
          {session.data.organizations.some((o) => o.role === "operator") && (
            <NavLink
              className="nav-item"
              to="/fleet"
              onClick={() => setMenuOpen(false)}
            >
              <NavIcon name="fleet" />
              Fleet operations
            </NavLink>
          )}
          <a className="nav-item explorer-link" href="/docs/">
            <NavIcon name="code" />
            API explorer
          </a>
        </nav>
      </aside>
      <div className="workspace">
        <header className="topbar">
          <span>{session.data.organizations[0]?.name ?? "Your workspace"}</span>
          <div>
            <span className="sim-badge">SIMULATED EQUIPMENT</span>
            <button
              className="text-button"
              onClick={() => logout.mutate()}
              disabled={logout.isPending}
            >
              Sign out
            </button>
            <ThemeToggle />
          </div>
        </header>
        <main id="main" tabIndex={-1}>
          <Notice error={logout.error} />
          <Routes>
            <Route
              path="/connect"
              element={
                <Suspense fallback={<Loading />}>
                  <Onboarding />
                </Suspense>
              }
            />
            <Route
              path="/fleet"
              element={
                <Suspense fallback={<Loading />}>
                  <Fleet session={session.data} />
                </Suspense>
              }
            />
            <Route path="/" element={<Overview session={session.data} />} />
            <Route path="/buildings/:buildingId" element={<BuildingPage />} />
            <Route
              path="*"
              element={
                <>
                  <h1>Page not found</h1>
                  <Link to="/">Back to buildings</Link>
                </>
              }
            />
          </Routes>
        </main>
      </div>
    </div>
  );
}

const loginSchema = z.object({
  login: z.email("Enter a valid email address."),
  password: z.string().min(1, "Enter your password."),
});
function Login() {
  const [register, setRegister] = useState(false);
  const client = useQueryClient();
  const form = useForm<z.infer<typeof loginSchema>>({
    resolver: zodResolver(loginSchema),
    defaultValues: { login: "owner@thermora.local", password: "" },
  });
  const login = useMutation({
    mutationFn: async (body: z.infer<typeof loginSchema>) =>
      register
        ? unwrap(await api.POST("/api/v1/accounts", { body }))
        : unwrap(await api.POST("/api/v1/session", { body })),
    onSuccess: (data) => client.setQueryData(["session"], data),
  });
  return (
    <main className="login-layout">
      <ThemeToggle />
      <section className="login-intro">
        <div className="brand">
          <Mark />
          Thermora
        </div>
        <div>
          <h1>
            Building heat,
            <br />
            under your control.
          </h1>
          <p>
            See your building respond. Understand every command. Keep comfort in
            your hands.
          </p>
        </div>
        <span className="sim-badge">INDEPENDENT REFERENCE PROJECT</span>
      </section>
      <section className="login">
        <h2>
          {register
            ? "Create your Thermora Cloud account"
            : "Sign in to Thermora"}
        </h2>
        <p className="muted">
          {register
            ? "Use a password of at least 12 characters. This local account is separate from your manufacturer account."
            : "Sign in to connect your simulated heat pump."}
        </p>
        <form
          onSubmit={(event) =>
            void form.handleSubmit((data) => login.mutate(data))(event)
          }
        >
          <label>
            Email
            <input autoComplete="username" {...form.register("login")} />
          </label>
          {form.formState.errors.login && (
            <p role="alert">{form.formState.errors.login.message}</p>
          )}
          <label>
            Password
            <input
              type="password"
              autoComplete={register ? "new-password" : "current-password"}
              minLength={register ? 12 : 1}
              maxLength={256}
              {...form.register("password")}
            />
          </label>
          {form.formState.errors.password && (
            <p role="alert">{form.formState.errors.password.message}</p>
          )}
          <Notice error={login.error} />
          <button type="submit" disabled={login.isPending}>
            {login.isPending
              ? "Please wait…"
              : register
                ? "Create account"
                : "Sign in"}
          </button>
        </form>
        <button
          className="text-button"
          type="button"
          disabled={login.isPending}
          onClick={() => {
            setRegister(!register);
            login.reset();
            form.reset({ login: "", password: "" });
          }}
        >
          {register
            ? "Already have an account? Sign in"
            : "Create a new account"}
        </button>
        <div className="footnote">
          Use your Thermora account, separate from the manufacturer account.
          <details>
            <summary>Local demo access</summary>
            <p>
              Run <code>task cloud:access</code> to find the private access
              files for your running profile. This environment controls
              simulated equipment only.
            </p>
          </details>
        </div>
      </section>
    </main>
  );
}

function Overview({ session }: { session: Session }) {
  const org = session.organizations[0];
  const client = useQueryClient();
  const navigate = useNavigate();
  const buildings = useQuery({
    queryKey: ["buildings", org?.id],
    enabled: !!org,
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/organizations/{organization_id}/buildings", {
          params: { path: { organization_id: org.id } },
        }),
      ),
    refetchInterval: 5000,
  });
  const create = useMutation({
    mutationFn: async (body: { name: string; timezone: string }) =>
      unwrap(
        await api.POST("/api/v1/organizations/{organization_id}/buildings", {
          params: { path: { organization_id: org.id }, header: newRequest() },
          body,
        }),
      ),
    onSuccess: (b) => {
      void client.invalidateQueries({ queryKey: ["buildings"] });
      void navigate(`/buildings/${b.id}`);
    },
  });
  if (!org)
    return (
      <section className="panel">
        <h1>Welcome to Thermora Cloud</h1>
        <p>
          Connect your manufacturer account to choose a heat pump and add your
          first building.
        </p>
        <Link to="/connect">Connect heat pump</Link>
      </section>
    );
  return (
    <>
      <div className="page-heading">
        <div>
          <h1>Buildings</h1>
          <p className="muted">
            Connect equipment and see what is happening inside your building.
          </p>
        </div>
        <Link className="button-link" to="/connect">
          Connect equipment
        </Link>
      </div>
      <Notice error={buildings.error} />
      {buildings.isPending && <Loading />}
      <div className="overview-grid">
        {buildings.data?.map((b) => (
          <Link
            className="building-card panel"
            to={`/buildings/${b.id}`}
            key={b.id}
          >
            <h2>{b.name}</h2>
            <p>{b.preferences.timezone}</p>
            <div className="card-bottom">
              <span>
                {b.devices.length
                  ? `${b.devices.length} heat pump connected`
                  : "Ready to connect equipment"}
              </span>
              <span aria-hidden="true">↗</span>
            </div>
          </Link>
        ))}
      </div>
      {!org ? (
        <p className="notice">Your account has no organization access.</p>
      ) : (
        <section className="panel create-building">
          <div>
            <h2>Add a building</h2>
            <p className="muted">Create the place your heat pump belongs to.</p>
          </div>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              const f = new FormData(e.currentTarget);
              const name = f.get("name");
              const timezone = f.get("timezone");
              if (typeof name === "string" && typeof timezone === "string") {
                create.mutate({ name, timezone });
              }
            }}
          >
            <label>
              Building name
              <input
                name="name"
                required
                maxLength={100}
                placeholder="e.g. Lakeside home"
              />
            </label>
            <label>
              Local timezone
              <input
                name="timezone"
                defaultValue="Europe/Helsinki"
                required
                maxLength={64}
              />
            </label>
            <Notice error={create.error} />
            <button disabled={create.isPending}>
              {create.isPending ? "Creating…" : "Create building →"}
            </button>
          </form>
        </section>
      )}
    </>
  );
}

function BuildingPage() {
  const { buildingId = "" } = useParams();
  const [search, setSearch] = useSearchParams();
  const deviceId = search.get("device") ?? "";
  const views = ["status", "schedule", "statistics", "activity", "settings"];
  const requestedView = search.get("view") ?? "status";
  const view = views.includes(requestedView) ? requestedView : "status";
  const building = useQuery({
    queryKey: ["building", buildingId],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/buildings/{building_id}", {
          params: { path: { building_id: buildingId } },
        }),
      ),
    refetchInterval: 2000,
  });
  if (building.isPending) return <Loading />;
  if (!building.data)
    return (
      <>
        <Link to="/">← Buildings</Link>
        <Notice error={building.error} />
      </>
    );
  const b = building.data;
  const selected = b.devices.find((d) => d.id === deviceId) ?? b.devices[0];
  return (
    <>
      <Link className="breadcrumb" to="/">
        Buildings / <span>{b.name}</span>
      </Link>
      <div className="page-heading">
        <div>
          <h1>{b.name}</h1>
          <p className="muted">
            {b.preferences.timezone} <span aria-hidden="true">·</span> Simulated
            space heating
          </p>
        </div>
        {b.devices.length > 0 && (
          <label>
            Heating zone
            <select
              value={selected?.id ?? ""}
              onChange={(e) => {
                const next = new URLSearchParams(search);
                next.set("device", e.target.value);
                setSearch(next);
              }}
            >
              {b.devices.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>
      <Notice error={building.error} />
      {deviceId && !b.devices.some((d) => d.id === deviceId) && (
        <p className="notice" role="status">
          That heating zone is unavailable in this building. Showing the first
          available zone.
        </p>
      )}
      <div className="building-navigation">
        <nav aria-label="Building views">
          {views.map((item) => (
            <Link
              key={item}
              to={`?view=${item}${selected ? `&device=${encodeURIComponent(selected.id)}` : ""}`}
              aria-current={view === item ? "page" : undefined}
            >
              {item[0].toUpperCase() + item.slice(1)}
            </Link>
          ))}
        </nav>
      </div>
      {selected ? (
        <>
          {view === "statistics" ? (
            <Suspense fallback={<Loading />}>
              <History
                key={`history-${selected.id}-${selected.run_id}`}
                device={selected}
                building={b}
              />
            </Suspense>
          ) : view === "schedule" ? (
            <Suspense fallback={<Loading />}>
              <Optimization key={selected.id} device={selected} building={b} />
            </Suspense>
          ) : view === "activity" ? (
            <Activity key={selected.id} device={selected} />
          ) : view === "settings" ? (
            <div className="settings-stack">
              <p>
                <Link to={`/connect?building=${b.id}`}>
                  Connect another heat pump to this building →
                </Link>
              </p>
              <BuildingSettings building={b} />
              <Preferences building={b} />
              <DeviceSettings
                key={`settings-${selected.id}`}
                device={selected}
              />
              <DeviceConnection
                key={`connection-${selected.id}`}
                device={selected}
              />
              <RunControls key={`run-${selected.id}`} device={selected} />
            </div>
          ) : (
            <DeviceDetail
              key={`${selected.id}-${selected.run_id}`}
              building={b}
              device={selected}
            />
          )}
        </>
      ) : (
        <Enroll building={b} />
      )}
    </>
  );
}

function Enroll({ building }: { building: Building }) {
  const client = useQueryClient();
  const enroll = useMutation({
    mutationFn: async (claim: string) =>
      unwrap(
        await api.POST("/api/v1/buildings/{building_id}/enrollments", {
          params: { path: { building_id: building.id }, header: newRequest() },
          body: { claim_code: claim },
        }),
      ),
    onSuccess: () =>
      client.invalidateQueries({ queryKey: ["building", building.id] }),
  });
  return (
    <section className="panel enrollment">
      <div className="equipment-icon">
        <Mark />
      </div>
      <h2>Connect a heat pump</h2>
      <p>
        <Link className="button-link" to={`/connect?building=${building.id}`}>
          Connect manufacturer account
        </Link>
      </p>
      <h3>Or use a direct equipment claim</h3>
      <p className="muted">
        Each heat pump has its own one-time claim code. Copy the{" "}
        <code>claim_code</code> value from its private demo access file, not its{" "}
        <code>device_id</code>.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const claim = new FormData(e.currentTarget).get("claim");
          if (typeof claim === "string") enroll.mutate(claim);
        }}
      >
        <label>
          Device claim code
          <input
            name="claim"
            autoComplete="off"
            required
            minLength={20}
            maxLength={128}
            placeholder="Paste the claim_code value"
          />
        </label>
        <Notice error={enroll.error} />
        <button disabled={enroll.isPending}>
          {enroll.isPending ? "Connecting…" : "Connect heat pump →"}
        </button>
      </form>
      <p className="footnote">
        Codes expire after 10 minutes and can be used once.
      </p>
      <p className="footnote">
        An empty claim code means this pump is already claimed. For another
        building, provision a separate pump with{" "}
        <code>task demo:device:add DEVICE=your-name</code>, then use the code in{" "}
        <code>.local/devices/your-name/demo-access.json</code>.
      </p>
    </section>
  );
}

function DeviceDetail({
  building,
  device,
}: {
  building: Building;
  device: Device;
}) {
  const run = useRun(device.id);
  const path = { device_id: device.id };
  const series = useQuery({
    queryKey: ["series", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/series", {
          params: { path },
        }),
      ),
    refetchInterval: 2000,
  });
  const commands = useQuery({
    queryKey: ["commands", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/commands", {
          params: { path },
        }),
      ),
    refetchInterval: 2000,
  });
  const s = device.latest;
  const online = device.connectivity === "online";
  const time = (date: string) =>
    new Intl.DateTimeFormat("en-GB", {
      timeZone: building.preferences.timezone,
      hour: "2-digit",
      minute: "2-digit",
    }).format(new Date(date));
  return (
    <>
      <section className="status-strip">
        <div>
          <span className={`status-dot ${online ? "" : "amber"}`} />
          <strong>{device.name}</strong>
          <span className="muted">
            {device.enrollment === "active"
              ? device.connectivity
              : device.enrollment === "revoked"
                ? "Identity retired"
                : "Activation pending"}
          </span>
        </div>
        <span>
          {s
            ? `Simulation · ${time(s.observed_at)}`
            : "Waiting for first authenticated telemetry"}
          {" · "}
          {run.error
            ? "Run status unavailable"
            : !run.data
              ? "Loading run status…"
              : run.data.status === "pending"
                ? `Run change pending: ${run.data.action ?? "update"}`
                : `${run.data.paused ? "Paused" : "Running"} · ${run.data.rate}×`}
          {s &&
            ` · ${s.control_mode === "remote" ? "Remote control" : "Local baseline"}`}
        </span>
      </section>
      {!online && (
        <p className="notice" role="status">
          {device.enrollment === "revoked"
            ? "This identity is retired. Its historical observations remain available."
            : device.enrollment !== "active"
              ? "Ownership is established. Waiting for edge activation and the first equipment sample."
              : "Telemetry is stale. Controls require a fresh connection. Equipment falls back to its local baseline when remote authority expires."}
        </p>
      )}
      <div className="metrics">
        <Metric
          label="Indoor temperature"
          value={s?.indoor_temperature_c.toFixed(2)}
          unit="°C"
          detail={`Comfort ${building.preferences.comfort_min_c}–${building.preferences.comfort_max_c} °C`}
        />
        <Metric
          label="Electrical power"
          value={s && (s.electrical_power_w / 1000).toFixed(2)}
          unit="kW"
          detail={s ? `Compressor ${s.compressor_state}` : "Awaiting sample"}
        />
        <Metric
          label="Run energy"
          value={s && (s.electrical_energy_wh / 1000).toFixed(3)}
          unit="kWh"
          detail="Cumulative in this simulation run"
        />
        <Metric
          label="Supply temperature"
          value={s?.supply_temperature_c.toFixed(1)}
          unit="°C"
          detail={
            s
              ? `Target ${s.supply_setpoint_c.toFixed(1)} °C`
              : "Awaiting sample"
          }
        />
      </div>
      <div className="detail-grid">
        <div>
          <section className="panel chart-panel">
            <div className="section-heading">
              <div>
                <h2>Operating timeline</h2>
                <p className="muted">Observed in simulation</p>
              </div>
            </div>
            <Notice error={series.error} />
            <Suspense fallback={<p role="status">Loading charts…</p>}>
              <TelemetryCharts
                samples={series.data ?? []}
                building={building}
              />
            </Suspense>
          </section>
        </div>
        <div className="control-column">
          <Override device={device} building={building} />
          <section className="comfort-summary">
            <h3>Comfort range</h3>
            <p className="muted">Current room temperature limits</p>
            <p className="comfort-value">
              {building.preferences.comfort_min_c.toFixed(1)} °C <span>/</span>{" "}
              {building.preferences.comfort_max_c.toFixed(1)} °C
            </p>
            <Link
              className="button-link secondary"
              to={`?view=settings&device=${device.id}`}
            >
              Edit preferences
            </Link>
          </section>
        </div>
      </div>
      <section className="panel recent-activity">
        <div className="section-heading">
          <h2>Recent activity</h2>
          <Link to={`?view=activity&device=${device.id}`}>
            View all activity
          </Link>
        </div>
        <Notice error={commands.error} />
        {!commands.data?.length ? (
          <p className="muted">
            Commands appear after configuration is accepted.
          </p>
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Time · wall clock</th>
                  <th>Intention</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {commands.data.slice(0, 3).map((c) => (
                  <tr key={c.id}>
                    <td>{time(c.issued_at_wall)}</td>
                    <td>{c.reason_code.replaceAll("_", " ")}</td>
                    <td>
                      <span className={`pill stage-${c.stage}`}>
                        {stageLabel[c.stage]}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <details className="connection-disclosure">
        <summary>Connection & execution</summary>
        <section className="panel connection">
          <h3>Connection & execution</h3>
          <dl>
            <div>
              <dt>Desired configuration</dt>
              <dd>v{device.desired_version}</dd>
            </div>
            <div>
              <dt>Equipment reports</dt>
              <dd>v{device.reported_version}</dd>
            </div>
            <div>
              <dt>Control generation</dt>
              <dd>{device.generation}</dd>
            </div>
            <div>
              <dt>Last live receipt</dt>
              <dd>
                {device.last_received_at
                  ? new Date(device.last_received_at).toLocaleTimeString()
                  : "Waiting"}
              </dd>
            </div>
          </dl>
          {device.desired_version !== device.reported_version && (
            <p className="notice">
              Configuration is awaiting equipment readback.
            </p>
          )}
          <p className="footnote">
            Command history distinguishes delivery, acceptance, application and
            telemetry confirmation.
          </p>
        </section>
      </details>
    </>
  );
}

function Metric({
  label,
  value,
  unit,
  detail,
}: {
  label: string;
  value?: string;
  unit: string;
  detail: string;
}) {
  return (
    <section className="metric">
      <p className="metric-label">{label}</p>
      <p className="metric-value">
        {value ?? "—"}
        <span>{unit}</span>
      </p>
      <p className="muted">{detail}</p>
    </section>
  );
}

function Override({
  device,
  building,
}: {
  device: Device;
  building: Building;
}) {
  const client = useQueryClient();
  const [submitted, setSubmitted] = useState(false);
  const invalidate = () => {
    setSubmitted(true);
    void client.invalidateQueries({ queryKey: ["commands", device.id] });
  };
  const override = useMutation({
    mutationFn: async (body: {
      room_target_c: number;
      duration_sim_minutes: number;
    }) =>
      unwrap(
        await api.POST("/api/v1/devices/{device_id}/overrides", {
          params: { path: { device_id: device.id }, header: newRequest() },
          body,
        }),
      ),
    onMutate: () => setSubmitted(false),
    onSuccess: invalidate,
  });
  const cancel = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.DELETE("/api/v1/devices/{device_id}/override", {
          params: { path: { device_id: device.id } },
        }),
      ),
    onSuccess: () => {
      setSubmitted(false);
      void client.invalidateQueries();
    },
  });
  const ready =
    device.latest !== undefined &&
    device.enrollment === "active" &&
    device.control_connected !== false &&
    device.connectivity === "online" &&
    device.desired_version === device.reported_version;
  return (
    <section className="panel override-panel">
      <h2>Temperature override</h2>
      <p className="muted">Temporarily change your room target.</p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          override.mutate({
            room_target_c: Number(f.get("target")),
            duration_sim_minutes: Number(f.get("minutes")),
          });
        }}
      >
        <div className="form-row">
          <label>
            Room target · °C
            <input
              name="target"
              type="number"
              required
              min={building.preferences.comfort_min_c}
              max={building.preferences.comfort_max_c}
              step="0.1"
              defaultValue={building.preferences.comfort_target_c}
            />
          </label>
          <label>
            Simulated minutes
            <select name="minutes" defaultValue="30">
              <option value="15">15 minutes</option>
              <option value="30">30 minutes</option>
              <option value="60">60 minutes</option>
              <option value="120">120 minutes</option>
            </select>
          </label>
        </div>
        <Notice error={override.error ?? cancel.error} />
        {submitted && (
          <p className="notice" role="status">
            Override recorded. Follow execution in command history.
          </p>
        )}
        <div className="override-actions">
          <button disabled={!ready || override.isPending}>
            {override.isPending ? "Recording…" : "Apply override"}
          </button>
          <button
            className="text-button"
            type="button"
            disabled={cancel.isPending}
            onClick={() => cancel.mutate()}
          >
            End manual override
          </button>
        </div>
        {!ready && (
          <p className="footnote">
            Available once telemetry is fresh and configuration is applied.
          </p>
        )}
      </form>
    </section>
  );
}

function Preferences({ building }: { building: Building }) {
  const client = useQueryClient();
  const [saved, setSaved] = useState(false);
  const save = useMutation({
    mutationFn: async (body: Building["preferences"]) =>
      unwrap(
        await api.PUT("/api/v1/buildings/{building_id}/preferences", {
          params: {
            path: { building_id: building.id },
            header: { "If-Match": `"${building.version}"` },
          },
          body,
        }),
      ),
    onSuccess: () => {
      setSaved(true);
      void client.invalidateQueries({ queryKey: ["building", building.id] });
    },
  });
  return (
    <section className="panel">
      <h2>Your comfort range</h2>
      <form
        key={building.version}
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          save.mutate({
            timezone: building.preferences.timezone,
            comfort_target_c: Number(f.get("target")),
            comfort_min_c: Number(f.get("min")),
            comfort_max_c: Number(f.get("max")),
          });
        }}
      >
        <label>
          Baseline target · °C
          <input
            name="target"
            type="number"
            step="0.1"
            min="16"
            max="26"
            required
            defaultValue={building.preferences.comfort_target_c}
          />
        </label>
        <div className="form-row">
          <label>
            Minimum · °C
            <input
              name="min"
              type="number"
              step="0.1"
              min="16"
              max="25.5"
              required
              defaultValue={building.preferences.comfort_min_c}
            />
          </label>
          <label>
            Maximum · °C
            <input
              name="max"
              type="number"
              step="0.1"
              min="16.5"
              max="26"
              required
              defaultValue={building.preferences.comfort_max_c}
            />
          </label>
        </div>
        <p className="footnote">
          Saving ends any current override and applies the new comfort
          configuration.
        </p>
        <Notice error={save.error} />
        {saved && (
          <p className="notice" role="status">
            Preferences saved. Check configuration readback below.
          </p>
        )}
        <button className="secondary" disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save preferences"}
        </button>
      </form>
    </section>
  );
}

const stageLabel: Record<Command["stage"], string> = {
  persisted: "Recorded",
  published: "Published",
  received: "Received",
  accepted: "Accepted",
  applied: "Applied",
  confirmed: "Telemetry confirmed",
  rejected: "Rejected",
  expired: "Expired",
  superseded: "Superseded",
};
function CommandHistory({ commands }: { commands: Command[] }) {
  return (
    <section className="panel history">
      <div className="section-heading">
        <div>
          <h2>Command history</h2>
          <p className="muted">
            Delivery, acceptance, application and telemetry confirmation.
          </p>
        </div>
      </div>
      {!commands.length ? (
        <p className="notice">
          The first baseline command appears after configuration is accepted.
        </p>
      ) : (
        <div className="command-list">
          {commands.map((c) => (
            <details key={c.id} className="command">
              <summary>
                <span className="command-number">
                  {String(c.sequence).padStart(2, "0")}
                </span>
                <span className="command-title">
                  <strong>{c.reason_code.replaceAll("_", " ")}</strong>
                  <small>
                    Room {c.room_target_c.toFixed(1)} °C · Supply{" "}
                    {c.supply_setpoint_c.toFixed(1)} °C
                  </small>
                </span>
                <span className={`pill stage-${c.stage}`}>
                  {stageLabel[c.stage]}
                </span>
              </summary>
              <div className="command-evidence">
                <p>
                  <code>{c.id}</code>
                </p>
                <p>
                  Recorded at {new Date(c.issued_at_wall).toLocaleTimeString()}{" "}
                  wall time. Valid until {new Date(c.valid_until).toISOString()}{" "}
                  simulated time.
                </p>
                <ol>
                  {c.events.map((e, i) => (
                    <li key={`${e.stage}-${i}`}>
                      <strong>{e.stage}</strong> ·{" "}
                      {new Date(e.occurred_at_wall).toLocaleTimeString()}
                      {e.reason_code ? ` · ${e.reason_code}` : ""}
                    </li>
                  ))}
                </ol>
                <p className="footnote">
                  Applied means register readback matched. Telemetry
                  confirmation verifies reported control state; the charts show
                  the subsequent thermal response.
                </p>
              </div>
            </details>
          ))}
        </div>
      )}
    </section>
  );
}

function Activity({ device }: { device: Device }) {
  const commands = useQuery({
    queryKey: ["commands", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/commands", {
          params: { path: { device_id: device.id } },
        }),
      ),
    refetchInterval: 2000,
  });
  return (
    <>
      <Notice error={commands.error} />
      {commands.isPending && <Loading />}
      <CommandHistory commands={commands.data ?? []} />
    </>
  );
}

function NavIcon({
  name,
}: {
  name: "building" | "link" | "fleet" | "code" | "menu";
}) {
  const paths = {
    building: "M3 21V8l9-5 9 5v13M1 21h22M9 21v-6h6v6M7 9h2m6 0h2M7 12h2m6 0h2",
    link: "m9 15 6-6M8 16l-1 1a4 4 0 0 1-6-6l4-4a4 4 0 0 1 6 0m2 10a4 4 0 0 0 6 0l4-4a4 4 0 0 0-6-6l-1 1",
    fleet: "M4 20V10m8 10V4m8 16v-8M1 23h22",
    code: "m8 5-6 7 6 7m8-14 6 7-6 7m-3-16-2 18",
    menu: "M3 6h18M3 12h18M3 18h18",
  };
  return (
    <svg
      aria-hidden="true"
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d={paths[name]} />
    </svg>
  );
}
