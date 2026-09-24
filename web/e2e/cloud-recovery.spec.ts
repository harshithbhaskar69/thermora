import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import { promisify } from "node:util";
import { randomUUID } from "node:crypto";
import type { components } from "../src/api/schema.gen";

test("broker and database outages recover buffered equipment observations", async ({
  request,
}) => {
  test.skip(
    process.env.THERMORA_RECOVERY_TEST !== "1",
    "Use task test:recovery after task test:cloud; this briefly stops local dependencies.",
  );
  test.setTimeout(300_000);
  const root = new URL("../../.local/cloud/devices/e2e/", import.meta.url);
  const account = JSON.parse(
    await readFile(new URL("browser-account.json", root), "utf8"),
  ) as { login: string; password: string };
  const seed = JSON.parse(
    await readFile(new URL("seed.json", root), "utf8"),
  ) as { device_id: string };
  expect(
    (await request.post("/api/v1/session", { data: account })).status(),
  ).toBe(200);
  const base = `/api/v1/devices/${seed.device_id}`;
  expect(
    (
      await request.put(base + "/optimization", {
        headers: { "Idempotency-Key": randomUUID() },
        data: { enabled: false, scenario: "winter" },
      })
    ).status(),
  ).toBe(202);
  expect((await request.delete(base + "/override")).status()).toBe(204);
  const execute = promisify(execFile);
  const kubectl = (...args: string[]) =>
    execute(
      "docker",
      [
        "exec",
        "floci-eks-thermora-cloud",
        "kubectl",
        "-n",
        "thermora",
        ...args,
      ],
      { timeout: 70_000 },
    );
  for (const dependency of ["mqtt", "postgres"]) {
    await test.step(`${dependency} outage and store-and-forward`, async () => {
      await expect
        .poll(
          async () => {
            const response = await request.get(base);
            if (response.status() !== 200) return false;
            const device =
              (await response.json()) as components["schemas"]["Device"];
            return (
              device.connectivity === "online" &&
              device.latest?.control_mode === "baseline" &&
              device.desired_version === device.reported_version
            );
          },
          { timeout: 60_000 },
        )
        .toBe(true);
      const before = (await (
        await request.get(base)
      ).json()) as components["schemas"]["Device"];
      let offlineAt = Date.now();
      let restored = offlineAt;
      try {
        await kubectl("scale", `deployment/${dependency}`, "--replicas=0");
        await kubectl(
          "wait",
          "--for=delete",
          "pod",
          "-l",
          `app=${dependency}`,
          "--timeout=60s",
        );
        offlineAt = Date.now();
        await expect
          .poll(async () => (await request.get("/health/ready")).status(), {
            timeout: 30_000,
          })
          .not.toBe(204);
        // Leave the simulator running long enough to produce samples during the outage.
        await expect
          .poll(() => Date.now() - offlineAt, {
            timeout: 15_000,
            intervals: [1000],
          })
          .toBeGreaterThanOrEqual(12_000);
      } finally {
        restored = Date.now();
        await kubectl("scale", `deployment/${dependency}`, "--replicas=1");
        await kubectl(
          "rollout",
          "status",
          `deployment/${dependency}`,
          "--timeout=60s",
        );
      }
      await expect
        .poll(async () => (await request.get("/health/ready")).status(), {
          timeout: 60_000,
        })
        .toBe(204);
      await expect
        .poll(
          async () => {
            const device = (await (
              await request.get(base)
            ).json()) as components["schemas"]["Device"];
            expect(device.run_id).toBe(before.run_id);
            return (
              device.connectivity === "online" &&
              device.latest?.control_mode === "baseline" &&
              Date.parse(device.latest.recorded_at_wall) > restored &&
              device.latest.sim_elapsed_seconds >
                (before.latest?.sim_elapsed_seconds ?? 0)
            );
          },
          { timeout: 60_000 },
        )
        .toBe(true);
      await expect
        .poll(
          async () => {
            const response = await request.get(base + "/series");
            expect(response.status()).toBe(200);
            const samples =
              (await response.json()) as components["schemas"]["Sample"][];
            return samples.some((sample) => {
              const recorded = Date.parse(sample.recorded_at_wall);
              return recorded > offlineAt + 2000 && recorded < restored - 2000;
            });
          },
          { timeout: 60_000 },
        )
        .toBe(true);
    });
  }
});
