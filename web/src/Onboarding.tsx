import { useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, newRequest, unwrap } from "./api/client";
import type { components } from "./api/schema.gen";

type Enrollment = components["schemas"]["CloudEnrollment"];

function ErrorNotice({ error }: { error: unknown }) {
  return error ? (
    <p className="notice error" role="alert">
      {error instanceof Error
        ? error.message
        : "Unable to continue. Try again."}
    </p>
  ) : null;
}

export default function Onboarding() {
  const [params] = useSearchParams();
  const [chosen, setChosen] = useState("");
  const [detailsFor, setDetailsFor] = useState("");
  const existingBuilding =
    params.get("building") ??
    sessionStorage.getItem("thermora-attach-building") ??
    "";
  const targetBuilding = useQuery({
    queryKey: ["building", existingBuilding],
    enabled: !!existingBuilding,
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/buildings/{building_id}", {
          params: { path: { building_id: existingBuilding } },
        }),
      ),
    retry: false,
  });

  const submission = useRef<{
    payload: string;
    headers: ReturnType<typeof newRequest>;
  } | null>(null);
  const client = useQueryClient();
  const navigate = useNavigate();
  const manufacturers = useQuery({
    queryKey: ["manufacturers"],
    queryFn: async () => unwrap(await api.GET("/api/v1/manufacturers")),
  });
  const connections = useQuery({
    queryKey: ["manufacturer-connections"],
    queryFn: async () =>
      unwrap(await api.GET("/api/v1/manufacturer-connections")),
  });
  const connection =
    chosen || params.get("connection") || connections.data?.at(-1)?.id || "";
  const devices = useQuery({
    queryKey: ["manufacturer-devices", connection],
    enabled: !!connection,
    queryFn: async () =>
      unwrap(
        await api.GET(
          "/api/v1/manufacturer-connections/{connection_id}/devices",
          { params: { path: { connection_id: connection } } },
        ),
      ),
    retry: false,
  });
  const authorize = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/api/v1/manufacturer-authorizations", {
          body: { manufacturer_id: "demo" },
        }),
      ),
    onSuccess: (data) => {
      if (existingBuilding)
        sessionStorage.setItem("thermora-attach-building", existingBuilding);
      window.location.assign(data.authorization_url);
    },
  });
  const complete = useMutation({
    mutationFn: async (body: Enrollment) => {
      const payload = JSON.stringify(body);
      if (submission.current?.payload !== payload) {
        submission.current = { payload, headers: newRequest() };
      }
      return unwrap(
        await api.POST("/api/v1/cloud-enrollments", {
          body,
          params: { header: submission.current.headers },
        }),
      );
    },
    onSuccess: async (building) => {
      submission.current = null;
      await client.invalidateQueries({ queryKey: ["session"] });
      await client.invalidateQueries({ queryKey: ["buildings"] });
      await client.invalidateQueries({ queryKey: ["manufacturer-devices"] });
      void navigate(`/buildings/${building.id}`);
    },
  });
  const attachSubmission = useRef<{
    payload: string;
    headers: ReturnType<typeof newRequest>;
  } | null>(null);
  const attach = useMutation({
    mutationFn: async (body: {
      connection_id: string;
      manufacturer_device_id: string;
      zone_name: string;
      consent_accepted: boolean;
    }) => {
      const payload = JSON.stringify({ building: existingBuilding, ...body });
      if (attachSubmission.current?.payload !== payload)
        attachSubmission.current = { payload, headers: newRequest() };
      return unwrap(
        await api.POST(
          "/api/v1/buildings/{building_id}/manufacturer-enrollments",
          {
            params: {
              path: { building_id: existingBuilding },
              header: attachSubmission.current.headers,
            },
            body,
          },
        ),
      );
    },
    onSuccess: async () => {
      attachSubmission.current = null;
      sessionStorage.removeItem("thermora-attach-building");
      await client.invalidateQueries({
        queryKey: ["building", existingBuilding],
      });
      void navigate(`/buildings/${existingBuilding}`);
    },
  });
  const disconnect = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.DELETE("/api/v1/manufacturer-connections/{connection_id}", {
          params: { path: { connection_id: connection } },
        }),
      ),
    onSuccess: async () => {
      await client.invalidateQueries({
        queryKey: ["manufacturer-connections"],
      });
      await client.invalidateQueries({ queryKey: ["manufacturer-devices"] });
    },
  });
  const callbackError = params.has("error");
  const available = devices.data?.filter((d) => d.available) ?? [];
  const showDetails = detailsFor === connection && !!connection;
  const continueToBuilding = (form: HTMLFormElement) => {
    const controls = form.querySelectorAll<
      HTMLInputElement | HTMLSelectElement
    >("[data-equipment] input, [data-equipment] select");
    if ([...controls].every((control) => control.reportValidity()))
      setDetailsFor(connection);
  };
  return (
    <>
      <div className="page-heading">
        <div>
          <h1>Connect to Thermora Cloud</h1>
          <p className="muted">
            Authorize your manufacturer account, choose your heat pump, then add
            your building.
          </p>
        </div>
      </div>
      <ol className="onboarding-steps" aria-label="Connection progress">
        <li aria-current={!connection ? "step" : undefined}>
          <span>1</span>Manufacturer account
        </li>
        <li aria-current={connection && !showDetails ? "step" : undefined}>
          <span>2</span>Select equipment
        </li>
        <li aria-current={showDetails ? "step" : undefined}>
          <span>3</span>Building & consent
        </li>
      </ol>
      {callbackError && (
        <p role="alert" className="notice error">
          Manufacturer authorization was cancelled or could not complete. Start
          again below.
        </p>
      )}
      <ErrorNotice
        error={manufacturers.error ?? connections.error ?? authorize.error}
      />
      {manufacturers.isPending && (
        <p role="status">Loading available manufacturers…</p>
      )}
      {manufacturers.data?.length === 0 && (
        <p className="notice">
          The local manufacturer has not been configured. Ask the demo operator
          to start cloud onboarding.
        </p>
      )}
      <section
        className={
          connection
            ? "panel onboarding-section connected-account"
            : "panel onboarding-section"
        }
        aria-labelledby="manufacturer-heading"
      >
        <h2 id="manufacturer-heading">Manufacturer account</h2>
        {manufacturers.data?.map((manufacturer) => (
          <div key={manufacturer.id}>
            <h3>{manufacturer.name}</h3>
            {!connection && <p>{manufacturer.prerequisites}</p>}
            <button
              disabled={authorize.isPending}
              onClick={() => authorize.mutate()}
            >
              {authorize.isPending
                ? "Opening manufacturer…"
                : connection
                  ? "Reconnect manufacturer account"
                  : "Connect manufacturer account"}
            </button>
          </div>
        ))}
        {!!connections.data?.length && (
          <label>
            Connected account
            <select
              value={connection}
              onChange={(event) => setChosen(event.target.value)}
            >
              {connections.data.map((c, index) => (
                <option value={c.id} key={c.id}>
                  Demo household {index + 1} · {c.status.replaceAll("_", " ")}
                </option>
              ))}
            </select>
          </label>
        )}
      </section>
      {connection && (
        <details className="account-access">
          <summary>Manage account access</summary>
          <p>
            Revoking this account disconnects every pump linked through it.
            History stays available.
          </p>
          <button
            disabled={disconnect.isPending}
            onClick={() => disconnect.mutate()}
          >
            Revoke manufacturer access
          </button>
          <ErrorNotice error={disconnect.error} />
          {disconnect.isSuccess && (
            <p role="status">
              Access withdrawn. Provider revocation will retry if the
              manufacturer is unavailable.
            </p>
          )}
        </details>
      )}
      {connection && (
        <section
          className="panel onboarding-section"
          aria-labelledby="building-heading"
        >
          <h2 id="building-heading">
            {existingBuilding
              ? "Connect to an existing building"
              : showDetails
                ? "Building details"
                : "Choose your heat pump"}
          </h2>
          {devices.isPending && (
            <p role="status">Discovering your heat pumps…</p>
          )}
          <ErrorNotice error={devices.error} />
          {devices.error && (
            <button onClick={() => void devices.refetch()}>
              Retry discovery
            </button>
          )}
          {devices.data && available.length === 0 && (
            <p className="notice">
              No unconnected heat pumps are available in this manufacturer
              account. Previously connected equipment remains in{" "}
              <Link to="/">your buildings</Link>. The demo operator can
              provision separate equipment for another building.
            </p>
          )}
          {existingBuilding && (
            <>
              <ErrorNotice error={targetBuilding.error} />
              <p>
                Connect to{" "}
                {targetBuilding.data?.name ?? "the selected building"}. Its
                current comfort and tariff settings apply. Each pump models an
                independent zone.
              </p>
              <button
                onClick={() => {
                  sessionStorage.removeItem("thermora-attach-building");
                  void navigate("/connect");
                }}
              >
                Create a new building instead
              </button>
            </>
          )}
          {existingBuilding && targetBuilding.data && available.length > 0 && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                const f = new FormData(e.currentTarget);
                attach.mutate({
                  connection_id: connection,
                  manufacturer_device_id:
                    typeof f.get("device") === "string"
                      ? (f.get("device") as string)
                      : "",
                  zone_name:
                    typeof f.get("zone") === "string"
                      ? (f.get("zone") as string)
                      : "",
                  consent_accepted: f.get("consent") === "on",
                });
              }}
            >
              <label>
                Heat pump
                <select name="device" required>
                  {available.map((d) => (
                    <option key={d.id} value={d.id}>
                      {d.name}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Zone name
                <input name="zone" required maxLength={100} />
              </label>
              <label>
                <input name="consent" type="checkbox" required /> I accept the
                local simulation terms and authorize this device.
              </label>
              <ErrorNotice error={attach.error} />
              <button disabled={attach.isPending}>Connect to building</button>
            </form>
          )}
          {available.length > 0 && !existingBuilding && (
            <form
              onSubmit={(event) => {
                event.preventDefault();
                if (!showDetails) {
                  continueToBuilding(event.currentTarget);
                  return;
                }
                const form = new FormData(event.currentTarget);
                const value = (key: string) => {
                  const entry = form.get(key);
                  return typeof entry === "string" ? entry.trim() : "";
                };
                complete.mutate({
                  connection_id: connection,
                  manufacturer_device_id: value("device"),
                  access_code: value("access_code"),
                  building_name: value("name"),
                  timezone: value("timezone"),
                  profile: {
                    floor_area_m2: Number(value("area")),
                    country_code: value("country"),
                    address: value("address"),
                    postal_code: value("postal"),
                    city: value("city"),
                    tariff_type:
                      value("tariff") === "fixed" ? "fixed" : "variable",
                  },
                  consent_version: "local-demo-2026-09",
                  consent_accepted: form.get("consent") === "on",
                });
              }}
            >
              <fieldset data-equipment hidden={showDetails}>
                <legend>Equipment access</legend>
                <label>
                  Heat pump
                  <select name="device" required>
                    {available.map((d) => (
                      <option key={d.id} value={d.id}>
                        {d.name}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Partner access code
                  <input
                    name="access_code"
                    type="password"
                    autoComplete="off"
                    required
                    minLength={16}
                    maxLength={128}
                    aria-describedby="access-help"
                  />
                </label>
                <p id="access-help" className="footnote">
                  Provided by the local program operator. This is separate from
                  an equipment claim code.
                </p>
                <button
                  type="button"
                  onClick={(event) => {
                    if (event.currentTarget.form)
                      continueToBuilding(event.currentTarget.form);
                  }}
                >
                  Continue to building
                </button>
              </fieldset>
              <fieldset hidden={!showDetails} disabled={!showDetails}>
                <legend>Building and preferences</legend>
                <button
                  type="button"
                  className="text-button"
                  onClick={() => setDetailsFor("")}
                >
                  Back to equipment
                </button>
                <label>
                  Building name
                  <input
                    name="name"
                    required
                    maxLength={100}
                    autoComplete="off"
                  />
                </label>
                <label>
                  Floor area (m²)
                  <input
                    name="area"
                    type="number"
                    required
                    min={1}
                    max={10000}
                    step="any"
                  />
                </label>
                <label>
                  Country code
                  <input
                    name="country"
                    required
                    pattern="[A-Z]{2}"
                    maxLength={2}
                    defaultValue="FI"
                    autoComplete="country"
                    aria-describedby="country-help"
                  />
                </label>
                <p id="country-help" className="footnote">
                  Two-letter country code, for example FI. Use fictional address
                  details for this local simulation.
                </p>
                <label>
                  Street address
                  <input
                    name="address"
                    required
                    maxLength={200}
                    autoComplete="street-address"
                  />
                </label>
                <label>
                  Postal code
                  <input
                    name="postal"
                    required
                    maxLength={20}
                    autoComplete="postal-code"
                  />
                </label>
                <label>
                  City
                  <input
                    name="city"
                    required
                    maxLength={100}
                    autoComplete="address-level2"
                  />
                </label>
                <label>
                  Time zone
                  <input
                    name="timezone"
                    required
                    maxLength={100}
                    defaultValue="Europe/Helsinki"
                    aria-describedby="timezone-help"
                  />
                </label>
                <p id="timezone-help" className="footnote">
                  IANA time zone used to display observations, such as
                  Europe/Helsinki.
                </p>
                <label>
                  Electricity tariff
                  <select name="tariff" required defaultValue="">
                    <option value="" disabled>
                      Choose your tariff
                    </option>
                    <option value="fixed">Fixed price</option>
                    <option value="variable">Variable price</option>
                  </select>
                </label>
                <p className="footnote">
                  Variable tariffs use packaged demo prices. Fixed tariffs use
                  €0.15/kWh. You can enable scenario optimization after
                  connecting. Results describe simulated operation and do not
                  guarantee savings.
                </p>
                <details>
                  <summary>Local simulation terms and data use</summary>
                  <p>
                    Thermora stores your local account, building details,
                    manufacturer authorization, equipment telemetry and control
                    history in this development environment. You authorize
                    discovery and enrollment of the selected simulated heat
                    pump. Manual changes affect simulated equipment only. There
                    is no billing, external customer service, real equipment
                    control or savings guarantee. Terms version:
                    local-demo-2026-09.
                  </p>
                </details>
                <label>
                  <input name="consent" type="checkbox" required /> I accept the
                  local simulation terms and authorize connecting this device.
                </label>
              </fieldset>
              <ErrorNotice error={complete.error} />
              <button
                hidden={!showDetails}
                disabled={complete.isPending || !showDetails}
                type="submit"
              >
                {complete.isPending
                  ? "Connecting heat pump…"
                  : "Add to Thermora Cloud"}
              </button>
            </form>
          )}
        </section>
      )}
    </>
  );
}
