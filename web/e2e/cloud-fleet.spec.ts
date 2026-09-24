import { expect, test } from "@playwright/test";
import { readFile, writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import type { components } from "../src/api/schema.gen";

test("prepare and exercise the bounded manufacturer fleet", async ({
  page,
}, testInfo) => {
  test.skip(
    process.env.THERMORA_FLEET_TEST !== "1",
    "Use task cloud:fleet to provision and validate named fleet fixtures.",
  );
  test.setTimeout(360_000);
  const root = new URL("../../.local/cloud/", import.meta.url);
  const seed = JSON.parse(
    await readFile(new URL("seed.json", root), "utf8"),
  ) as { login: string; password: string; organization_id: string };
  const operator = JSON.parse(
    await readFile(new URL("operator-access.json", root), "utf8"),
  ) as { login: string; password: string };
  const access = JSON.parse(
    await readFile(new URL("onboarding-access.json", root), "utf8"),
  ) as { manufacturer_login: string; manufacturer_password: string };
  const login = await page.request.post("/api/v1/session", {
    data: { login: seed.login, password: seed.password },
  });
  expect(login.status()).toBe(200);
  await page.goto("/connect");
  await page
    .getByRole("button", { name: /^(Connect|Reconnect) manufacturer account$/ })
    .click();
  await page.getByLabel("Manufacturer email").fill(access.manufacturer_login);
  await page
    .getByLabel("Manufacturer password")
    .fill(access.manufacturer_password);
  await page.getByRole("button", { name: "Authorize device access" }).click();
  await expect(
    page.getByRole("heading", { name: "Connect to Thermora Cloud" }),
  ).toBeVisible();
  const connections = (await (
    await page.request.get("/api/v1/manufacturer-connections")
  ).json()) as components["schemas"]["ManufacturerConnection"][];
  const connection = connections.find((c) => c.status === "connected");
  expect(connection).toBeDefined();
  const count = Number(process.env.THERMORA_FLEET_COUNT ?? 10);
  expect(Number.isInteger(count) && count > 0 && count <= 25).toBeTruthy();
  const ids: string[] = [];
  for (let index = 1; index <= count; index++) {
    const name = `fleet-${String(index).padStart(2, "0")}`;
    const device = JSON.parse(
      await readFile(new URL(`devices/${name}/seed.json`, root), "utf8"),
    ) as { device_id: string };
    const invitation = JSON.parse(
      await readFile(
        new URL(`devices/${name}/onboarding-access.json`, root),
        "utf8",
      ),
    ) as { partner_access_code: string };
    ids.push(device.device_id);
    const existing = await page.request.get(
      `/api/v1/devices/${device.device_id}`,
    );
    if (existing.status() === 404) {
      const response = await page.request.post("/api/v1/cloud-enrollments", {
        headers: { "Idempotency-Key": randomUUID() },
        data: {
          connection_id: connection?.id,
          manufacturer_device_id: device.device_id,
          access_code: invitation.partner_access_code,
          building_name: `Fleet home ${index}`,
          timezone: "Europe/Helsinki",
          profile: {
            floor_area_m2: 120,
            country_code: "FI",
            address: `Fictional street ${index}`,
            postal_code: "00000",
            city: "Demo city",
            tariff_type: "variable",
          },
          consent_version: "local-demo-2026-09",
          consent_accepted: true,
        },
      });
      expect(response.status()).toBe(201);
    } else {
      expect(existing.status()).toBe(200);
    }
    await expect
      .poll(
        async () => {
          const r = await page.request.get(
            `/api/v1/devices/${device.device_id}`,
          );
          const d = (await r.json()) as components["schemas"]["Device"];
          return d.connectivity;
        },
        { timeout: 45_000 },
      )
      .toBe("online");
    const settings = (await (
      await page.request.get(`/api/v1/devices/${device.device_id}/settings`)
    ).json()) as components["schemas"]["DeviceSettings"];
    const update = await page.request.put(
      `/api/v1/devices/${device.device_id}/settings`,
      {
        headers: { "If-Match": `"${settings.version}"` },
        data: { ...settings.preferences, flexibility_opt_in: true },
      },
    );
    expect(update.status()).toBe(202);
    const optimize = await page.request.put(
      `/api/v1/devices/${device.device_id}/optimization`,
      {
        headers: { "Idempotency-Key": randomUUID() },
        data: { enabled: true, scenario: "winter" },
      },
    );
    expect(optimize.status()).toBe(202);
  }
  const operatorLogin = await page.request.post("/api/v1/session", {
    data: { login: operator.login, password: operator.password },
  });
  expect(operatorLogin.status()).toBe(200);
  const base = `/api/v1/organizations/${seed.organization_id}`;
  await expect
    .poll(
      async () => {
        const r = await page.request.get(base + "/fleet");
        const devices =
          (await r.json()) as components["schemas"]["FleetDevice"][];
        return devices.filter(
          (d) =>
            ids.includes(d.device.id) && d.device.connectivity === "online",
        ).length;
      },
      { timeout: 45_000 },
    )
    .toBe(count);
  const existingEvents = (await (
    await page.request.get(base + "/flexibility-events")
  ).json()) as components["schemas"]["FlexibilityEvent"][];
  for (const event of existingEvents.filter((e) => e.status === "active")) {
    expect(
      (
        await page.request.delete(base + "/flexibility-events/" + event.id)
      ).status(),
    ).toBe(204);
  }
  await page.goto("/fleet");
  await expect(
    page.getByRole("heading", { name: "Fleet operations" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Start simulated event" }),
  ).toBeEnabled({ timeout: 45_000 });
  const createdResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith(base + "/flexibility-events") &&
      response.request().method() === "POST",
  );
  await page.getByRole("button", { name: "Start simulated event" }).click();
  const response = await createdResponse;
  expect(response.status()).toBe(201);
  const created =
    (await response.json()) as components["schemas"]["FlexibilityEvent"];
  expect(existingEvents.map((event) => event.id)).not.toContain(created.id);
  await expect(
    page.getByRole("button", { name: "Cancel event" }),
  ).toBeVisible();
  await expect
    .poll(
      async () => {
        const detail = (await (
          await page.request.get(base + "/flexibility-events/" + created.id)
        ).json()) as components["schemas"]["FlexibilityEvent"];
        expect(detail.id).toBe(created.id);
        return detail.participants.some(
          (p) =>
            ids.includes(p.device_id) &&
            p.command_counts.some(
              (c) => c.stage === "confirmed" && c.count > 0,
            ),
        );
      },
      { timeout: 45_000 },
    )
    .toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("fleet-desktop.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "Cancel event" }).click();
  await expect(page.getByRole("button", { name: "Cancel event" })).toHaveCount(
    0,
  );
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBeTruthy();
  const metrics = (await (
    await page.request.get(base + "/metrics")
  ).json()) as components["schemas"]["Metrics"];
  expect(
    metrics.device_diagnostics.filter(
      (d) => ids.includes(d.device_id) && d.edge,
    ).length,
  ).toBe(count);
  await writeFile(
    new URL("fleet-evidence.json", root),
    JSON.stringify(
      {
        checked_at: new Date().toISOString(),
        devices: count,
        event_id: created.id,
        metrics,
      },
      null,
      2,
    ),
    { mode: 0o600 },
  );
});
