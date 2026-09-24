import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  api,
  unwrap,
  APIError,
  type Building,
  type Device,
} from "../api/client";
import type { components } from "../api/schema.gen";
import { Link } from "react-router";

type Profile = components["schemas"]["BuildingProfile"];

export function BuildingSettings({ building }: { building: Building }) {
  const [version, setVersion] = useState(building.version);
  const client = useQueryClient();
  const profile = useQuery({
    queryKey: ["profile", building.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/buildings/{building_id}/profile", {
          params: { path: { building_id: building.id } },
        }),
      ),
    retry: false,
  });
  const save = useMutation({
    mutationFn: async (body: { name: string; profile: Profile }) =>
      unwrap(
        await api.PUT("/api/v1/buildings/{building_id}/profile", {
          params: {
            path: { building_id: building.id },
            header: { "If-Match": `"${version}"` },
          },
          body,
        }),
      ),
    onSuccess: async (updated) => {
      setVersion(updated.version);
      await client.invalidateQueries({ queryKey: ["building", building.id] });
      await client.invalidateQueries({ queryKey: ["profile", building.id] });
    },
  });
  if (profile.isPending) return <p role="status">Loading building profile…</p>;
  if (
    profile.error &&
    !(profile.error instanceof APIError && profile.error.status === 404)
  )
    return <p role="alert">{profile.error.message}</p>;
  const p = profile.data;
  return (
    <section className="panel">
      <h2>Building settings</h2>
      <p className="muted">
        Address and area describe this building. They do not fetch weather or
        change the simulated zone model. Tariff changes invalidate future
        control plans.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          const value = (key: string) => {
            const entry = f.get(key);
            return typeof entry === "string" ? entry : "";
          };
          save.mutate({
            name: value("name"),
            profile: {
              floor_area_m2: Number(value("area")),
              country_code: value("country"),
              address: value("address"),
              postal_code: value("postal"),
              city: value("city"),
              tariff_type: value("tariff") === "fixed" ? "fixed" : "variable",
            },
          });
        }}
      >
        <label>
          Building name
          <input
            name="name"
            required
            maxLength={100}
            defaultValue={building.name}
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
            defaultValue={p?.floor_area_m2}
          />
        </label>
        <label>
          Country code
          <input
            name="country"
            required
            pattern="[A-Z]{2}"
            maxLength={2}
            defaultValue={p?.country_code ?? "FI"}
          />
        </label>
        <label>
          Street address
          <input
            name="address"
            required
            maxLength={200}
            defaultValue={p?.address}
          />
        </label>
        <label>
          Postal code
          <input
            name="postal"
            required
            maxLength={20}
            defaultValue={p?.postal_code}
          />
        </label>
        <label>
          City
          <input name="city" required maxLength={100} defaultValue={p?.city} />
        </label>
        <label>
          Electricity tariff
          <select name="tariff" defaultValue={p?.tariff_type ?? "variable"}>
            <option value="variable">Variable · packaged prices</option>
            <option value="fixed">Fixed · €0.15/kWh</option>
          </select>
        </label>
        {save.error && (
          <p role="alert" className="notice error">
            {save.error.message} Reopen settings after a version conflict.
          </p>
        )}
        {save.isSuccess && <p role="status">Building saved.</p>}
        <button disabled={save.isPending}>Save building</button>
      </form>
    </section>
  );
}

export function DeviceConnection({ device }: { device: Device }) {
  const [consent, setConsent] = useState(false);
  const client = useQueryClient();
  const change = useMutation({
    mutationFn: async () => {
      const params = { path: { device_id: device.id } };
      if (device.control_connected === false) {
        return unwrap(
          await api.PUT("/api/v1/devices/{device_id}/connection", {
            params,
            body: {
              connection_id: device.connection_id ?? "",
              consent_accepted: consent,
            },
          }),
        );
      }
      return unwrap(
        await api.DELETE("/api/v1/devices/{device_id}/connection", { params }),
      );
    },
    onSuccess: async () => {
      setConsent(false);
      await client.invalidateQueries({
        queryKey: ["building", device.building_id],
      });
      await client.invalidateQueries({ queryKey: ["optimization", device.id] });
    },
  });
  if (!device.connection_id)
    return (
      <section className="panel">
        <h2>Equipment identity</h2>
        <p>
          Retiring this direct-edge identity stops future control and revokes
          broker access. History is preserved. Re-enrollment requires a newly
          provisioned device identity.
        </p>
        {device.enrollment === "revoked" ? (
          <p role="status">
            Identity retired. Equipment returns to local baseline when its lease
            expires.
          </p>
        ) : (
          <>
            <label>
              <input
                type="checkbox"
                checked={consent}
                onChange={(e) => setConsent(e.target.checked)}
              />{" "}
              I understand that this identity cannot be reused.
            </label>
            <button
              className="secondary"
              disabled={!consent || change.isPending}
              onClick={() => change.mutate()}
            >
              Retire device identity
            </button>
          </>
        )}
        {change.error && <p role="alert">{change.error.message}</p>}
      </section>
    );
  return (
    <section className="panel">
      <h2>Device connection</h2>
      <p>
        {device.control_connected === false
          ? "Thermora control is disconnected. Ownership and history are retained."
          : "This pump is connected through your manufacturer account."}
      </p>
      <p className="muted">
        Disconnection stops new Thermora instructions. Existing equipment
        control falls back when its lease expires, within approximately 40 wall
        seconds; this is not a physical-stop confirmation.
      </p>
      {device.control_connected === false && (
        <label>
          <input
            type="checkbox"
            checked={consent}
            onChange={(e) => setConsent(e.target.checked)}
          />{" "}
          I accept the local simulation terms and authorize this device again.
        </label>
      )}
      {change.error && (
        <p role="alert" className="notice error">
          {change.error.message}{" "}
          <Link to="/connect">Manage manufacturer authorization</Link>
        </p>
      )}
      <button
        disabled={
          change.isPending || (device.control_connected === false && !consent)
        }
        onClick={() => change.mutate()}
      >
        {device.control_connected === false
          ? "Reconnect this heat pump"
          : "Disconnect this heat pump"}
      </button>
    </section>
  );
}
