import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import type { components } from "../src/api/schema.gen";

test("enrollment, authenticated telemetry, and a real manual control loop", async ({
  page,
}, testInfo) => {
  const access = JSON.parse(
    await readFile(
      new URL("../../.local/devices/e2e/demo-access.json", import.meta.url),
      "utf8",
    ),
  ) as {
    login: string;
    password: string;
    claim_code: string;
    device_id: string;
    organization_id: string;
  };
  const browserErrors: string[] = [];
  page.on("pageerror", (error) => browserErrors.push(error.message));
  await page.goto("/");
  await page.getByLabel("Email", { exact: true }).fill(access.login);
  await page.getByLabel("Password", { exact: true }).fill(access.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Buildings" })).toBeVisible();

  const existingResponse = await page.request.get(
    `/api/v1/devices/${access.device_id}`,
  );
  if (existingResponse.status() === 404) {
    expect(access.claim_code.length).toBeGreaterThan(20);
    await page.getByLabel("Building name").fill("Browser test building");
    await page.getByRole("button", { name: "Create building →" }).click();
    await page.getByLabel("Device claim code").fill(access.claim_code);
    await page.getByRole("button", { name: "Connect heat pump →" }).click();
  } else {
    expect(existingResponse.ok()).toBeTruthy();
    const existing =
      (await existingResponse.json()) as components["schemas"]["Device"];
    await page.goto(`/buildings/${existing.building_id}`);
  }
  await expect(
    page.getByRole("heading", { name: "Operating timeline" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Apply override" }),
  ).toBeEnabled({ timeout: 45_000 });
  const getDevice = async () => {
    const response = await page.request.get(
      `/api/v1/devices/${access.device_id}`,
    );
    expect(response.ok()).toBeTruthy();
    return response.json() as Promise<components["schemas"]["Device"]>;
  };
  const before = await getDevice();
  expect(before.origin).toBe("simulation");
  expect(before.latest?.outdoor_temperature_c).toBe(-3);
  expect(before.reported_version).toBe(before.desired_version);

  const commandResponse = page.waitForResponse(
    (r) =>
      r.url().endsWith(`/api/v1/devices/${access.device_id}/overrides`) &&
      r.request().method() === "POST",
  );
  await page.getByLabel("Room target · °C", { exact: true }).fill("22");
  await page.getByLabel("Simulated minutes").selectOption("120");
  await page.getByRole("button", { name: "Apply override" }).click();
  const response = await commandResponse;
  expect(response.status()).toBe(202);
  const command = (await response.json()) as components["schemas"]["Command"];

  await expect
    .poll(
      async () => {
        const result = await page.request.get(
          `/api/v1/devices/${access.device_id}/commands`,
        );
        const commands =
          (await result.json()) as components["schemas"]["Command"][];
        return commands.find((c) => c.id === command.id)?.stage;
      },
      { timeout: 30_000 },
    )
    .toBe("confirmed");
  const after = await getDevice();
  expect(after.latest?.control_mode).toBe("remote");
  expect(after.latest?.room_target_c).toBe(22);
  expect(after.latest?.applied_command_seq).toBe(command.sequence);
  expect(after.latest?.supply_setpoint_c).toBeCloseTo(
    command.supply_setpoint_c,
    1,
  );
  await expect
    .poll(async () => (await getDevice()).latest?.electrical_energy_wh, {
      timeout: 20_000,
    })
    .toBeGreaterThan(before.latest?.electrical_energy_wh ?? 0);

  await expect
    .poll(async () => {
      const commands = (await (
        await page.request.get(`/api/v1/devices/${access.device_id}/commands`)
      ).json()) as components["schemas"]["Command"][];
      return commands
        .find((c) => c.id === command.id)
        ?.events.map((e) => e.stage);
    })
    .toEqual(
      expect.arrayContaining(["received", "accepted", "applied", "confirmed"]),
    );
  await page.getByRole("link", { name: "Activity", exact: true }).click();
  await expect(page.locator(".command").first()).toContainText(
    "Telemetry confirmed",
  );
  await page.getByRole("link", { name: "Status", exact: true }).click();
  await page.screenshot({
    path: testInfo.outputPath("device-detail.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("heading", { name: "Operating timeline" }),
  ).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBeTruthy();
  await page.screenshot({
    path: testInfo.outputPath("device-mobile.png"),
    fullPage: true,
  });

  await page.getByRole("button", { name: "End manual override" }).click();
  await expect
    .poll(async () => (await getDevice()).latest?.control_mode, {
      timeout: 20_000,
    })
    .toBe("baseline");
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  const preferencesResponse = page.waitForResponse(
    (r) => r.url().endsWith("/preferences") && r.request().method() === "PUT",
  );
  await page.getByLabel("Baseline target · °C").fill("21.1");
  await page.getByRole("button", { name: "Save preferences" }).click();
  const preferences = await preferencesResponse;
  expect(preferences.status()).toBe(200);
  const updated =
    (await preferences.json()) as components["schemas"]["Building"];
  await expect
    .poll(async () => (await getDevice()).reported_version, { timeout: 20_000 })
    .toBe(updated.devices[0].desired_version);
  expect(browserErrors).toEqual([]);
});
