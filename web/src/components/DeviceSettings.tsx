import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, unwrap, type Device } from "../api/client";
import type { components } from "../api/schema.gen";

export function DeviceSettings({ device }: { device: Device }) {
  const client = useQueryClient();
  const settings = useQuery({
    queryKey: ["device-settings", device.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/settings", {
          params: { path: { device_id: device.id } },
        }),
      ),
    refetchInterval: 5000,
  });
  const save = useMutation({
    mutationFn: async (input: {
      version: number;
      body: components["schemas"]["HeatingPreferences"];
    }) =>
      unwrap(
        await api.PUT("/api/v1/devices/{device_id}/settings", {
          params: {
            path: { device_id: device.id },
            header: { "If-Match": `"${input.version}"` },
          },
          body: input.body,
        }),
      ),
    onSuccess: () => client.invalidateQueries(),
  });
  const current = settings.data;
  return (
    <section className="panel">
      <h2>Device preferences</h2>
      {(settings.error || save.error) && (
        <p role="alert">{(settings.error || save.error)?.message}</p>
      )}
      {!current ? (
        <p>Loading preferences…</p>
      ) : (
        <form
          key={current.version}
          onSubmit={(e) => {
            e.preventDefault();
            const form = new FormData(e.currentTarget);
            const preset = form.get("preset");
            save.mutate({
              version: current.version,
              body: {
                preset:
                  preset === "comfort"
                    ? "comfort"
                    : preset === "maximum-savings"
                      ? "maximum-savings"
                      : "balanced",
                flexibility_opt_in: form.get("flexibility") === "on",
                ...(current.preferences.hot_water
                  ? {
                      hot_water: {
                        start_c: Number(form.get("start")),
                        stop_c: Number(form.get("stop")),
                      },
                    }
                  : {}),
              },
            });
          }}
        >
          <label>
            Optimization preset
            <select name="preset" defaultValue={current.preferences.preset}>
              <option value="comfort">Comfort</option>
              <option value="balanced">Balanced</option>
              <option value="maximum-savings">Maximum Savings</option>
            </select>
          </label>
          <p className="muted">
            Comfort holds the room target. Balanced leaves 0.25 °C from the
            comfort limits when shifting heat; Maximum Savings leaves 0.05 °C.
            All presets keep the same equipment protections. Savings are not
            guaranteed.
          </p>
          {current.preferences.hot_water && (
            <fieldset>
              <legend>Domestic hot water</legend>
              <label>
                Start heating at · °C
                <input
                  name="start"
                  type="number"
                  min={40}
                  max={52}
                  step={0.5}
                  required
                  defaultValue={current.preferences.hot_water.start_c}
                />
              </label>
              <label>
                Stop heating at · °C
                <input
                  name="stop"
                  type="number"
                  min={43}
                  max={55}
                  step={0.5}
                  required
                  defaultValue={current.preferences.hot_water.stop_c}
                />
              </label>
              <p>
                Keep at least 3 °C between start and stop. These are simulated
                settings.
              </p>
            </fieldset>
          )}
          <label>
            <input
              name="flexibility"
              type="checkbox"
              defaultChecked={current.preferences.flexibility_opt_in}
            />{" "}
            Allow simulated partner flexibility events
          </label>
          <p className="muted">
            An owner override or opt-out takes priority over a partner event.
          </p>
          <button disabled={save.isPending}>Save device preferences</button>
          {save.isSuccess && (
            <p role="status">
              Preferences saved. Applied configuration is confirmed separately
              by the heat pump.
            </p>
          )}
        </form>
      )}
      {device.latest?.hot_water && (
        <dl className="metrics">
          <div>
            <dt>Hot water</dt>
            <dd>{device.latest.hot_water.temperature_c.toFixed(1)} °C</dd>
          </div>
          <div>
            <dt>Buffer tank</dt>
            <dd>{device.latest.buffer_temperature_c?.toFixed(1)} °C</dd>
          </div>
          <div>
            <dt>Hot-water electricity</dt>
            <dd>
              {(device.latest.hot_water.electrical_energy_wh / 1000).toFixed(3)}{" "}
              kWh
            </dd>
          </div>
        </dl>
      )}
    </section>
  );
}
