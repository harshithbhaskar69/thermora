import { useRef } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, newRequest, unwrap, type Device } from "../api/client";
import type { components } from "../api/schema.gen";

export function useRun(deviceID: string) {
  return useQuery({
    queryKey: ["run", deviceID],
    queryFn: async () =>
      unwrap(
        await api.GET("/api/v1/devices/{device_id}/run", {
          params: { path: { device_id: deviceID } },
        }),
      ),
    refetchInterval: 2000,
  });
}

export function RunControls({ device }: { device: Device }) {
  const client = useQueryClient();
  const attempt = useRef({ body: "", header: newRequest() });
  const run = useRun(device.id);
  const change = useMutation({
    mutationFn: async (body: {
      action: "pause" | "resume" | "rate" | "reset";
      rate: 1 | 10 | 60;
      model: components["schemas"]["RunState"]["model"];
    }) => {
      const encoded = JSON.stringify(body);
      if (attempt.current.body !== encoded)
        attempt.current = { body: encoded, header: newRequest() };
      return unwrap(
        await api.POST("/api/v1/devices/{device_id}/run", {
          params: {
            path: { device_id: device.id },
            header: attempt.current.header,
          },
          body,
        }),
      );
    },
    onSuccess: () => {
      attempt.current.body = "";
      return client.invalidateQueries();
    },
  });
  const state = run.data;
  return (
    <section className="panel">
      <h2>Run controls</h2>
      {(run.error || change.error) && (
        <p role="alert">{(run.error || change.error)?.message}</p>
      )}
      {!state ? (
        <p>Loading run…</p>
      ) : (
        <>
          <p role="status">
            Run {state.epoch} · {state.paused ? "Paused" : "Running"} ·{" "}
            {state.rate}× simulated time
            {state.status === "pending"
              ? ` · Waiting for equipment to acknowledge ${state.action}`
              : ""}
          </p>
          <fieldset
            disabled={
              change.isPending ||
              state.status === "pending" ||
              device.control_connected === false ||
              device.enrollment !== "active"
            }
          >
            <legend>Simulation pacing</legend>
            <button
              type="button"
              onClick={() =>
                change.mutate({
                  action: state.paused ? "resume" : "pause",
                  rate: state.rate === 1 || state.rate === 10 ? state.rate : 60,
                  model: state.model,
                })
              }
            >
              {state.paused ? "Resume simulation" : "Pause simulation"}
            </button>
            <label>
              Time speed
              <select
                value={state.rate}
                onChange={(e) =>
                  change.mutate({
                    action: "rate",
                    rate:
                      e.target.value === "1"
                        ? 1
                        : e.target.value === "10"
                          ? 10
                          : 60,
                    model: state.model,
                  })
                }
              >
                <option value={1}>1× · Real time</option>
                <option value={10}>10×</option>
                <option value={60}>60× · One minute per second</option>
              </select>
            </label>
            <details>
              <summary>Start a new run</summary>
              <p>
                This resets the simulated equipment and disables optimization.
                Earlier telemetry and commands remain in history.
              </p>
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  change.mutate({
                    action: "reset",
                    rate:
                      state.rate === 1 || state.rate === 10 ? state.rate : 60,
                    model:
                      new FormData(e.currentTarget).get("model") ===
                      "heating-dhw"
                        ? "heating-dhw"
                        : "heating",
                  });
                }}
              >
                <label>
                  Equipment model
                  <select name="model" defaultValue={state.model}>
                    <option value="heating">Space heating</option>
                    <option value="heating-dhw">
                      Space heating, buffer and hot water
                    </option>
                  </select>
                </label>
                <label>
                  <input type="checkbox" required /> I want to reset this
                  simulated heat pump.
                </label>
                <button>Reset simulation</button>
              </form>
            </details>
          </fieldset>
        </>
      )}
    </section>
  );
}
