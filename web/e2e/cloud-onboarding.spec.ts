import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { readFile, writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import type { components } from "../src/api/schema.gen";

const { manufacturer: manufacturerOrigin } =
  process.env.THERMORA_CLOUD_TEST === "1"
    ? (JSON.parse(
        readFileSync(
          new URL("../../.local/cloud/public-origins.json", import.meta.url),
          "utf8",
        ),
      ) as { manufacturer: string })
    : { manufacturer: "https://manufacturer.localhost:8444" };

test("cloud signup, manufacturer authorization, entitlement, consent and live telemetry", async ({
  page,
}) => {
  test.setTimeout(240_000);
  test.skip(
    process.env.THERMORA_CLOUD_TEST !== "1",
    "Use task test:cloud for the isolated cloud profile.",
  );
  const root = new URL("../../.local/cloud/devices/e2e/", import.meta.url);
  const access = JSON.parse(
    await readFile(new URL("onboarding-access.json", root), "utf8"),
  ) as {
    manufacturer_login: string;
    manufacturer_password: string;
    partner_access_code: string;
  };
  const seed = JSON.parse(
    await readFile(new URL("seed.json", root), "utf8"),
  ) as { device_id: string };
  const accountFile = new URL("browser-account.json", root);
  let account: { login: string; password: string };
  try {
    account = JSON.parse(await readFile(accountFile, "utf8")) as typeof account;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    account = {
      login: `browser-${randomUUID()}@thermora.local`,
      password: randomUUID() + randomUUID(),
    };
    await writeFile(accountFile, JSON.stringify(account), {
      mode: 0o600,
      flag: "wx",
    });
  }
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const login = await page.request.post("/api/v1/session", { data: account });
  if (login.status() === 401) {
    await page.goto("/connect");
    await page.getByRole("button", { name: "Create a new account" }).click();
    await page.getByLabel("Email", { exact: true }).fill(account.login);
    await page.getByLabel("Password", { exact: true }).fill(account.password);
    await page
      .getByRole("button", { name: "Create account", exact: true })
      .click();
  } else {
    expect(login.ok()).toBeTruthy();
    const saved = await page.request.get("/api/v1/manufacturer-connections");
    expect(saved.status()).toBe(200);
    const connections =
      (await saved.json()) as components["schemas"]["ManufacturerConnection"][];
    for (const connection of connections) {
      const discovered = await page.request.get(
        `/api/v1/manufacturer-connections/${connection.id}/devices`,
      );
      // A previously read-only grant may require the one-time control-scope upgrade.
      expect([200, 409]).toContain(discovered.status());
    }
    await page.goto("/connect");
  }
  await expect(
    page.getByRole("heading", { name: "Connect to Thermora Cloud" }),
  ).toBeVisible();
  const authorizationRequest = page.waitForRequest((request) =>
    request.url().startsWith(manufacturerOrigin + "/oauth/authorize"),
  );
  await page
    .getByRole("button", { name: /^(Connect|Reconnect) manufacturer account$/ })
    .click();
  await page.waitForURL(manufacturerOrigin + "/**");
  await expect(
    page.getByRole("heading", { name: "Connect your heat pump" }),
  ).toBeVisible();
  expect(new URL(page.url()).hostname).toBe(
    new URL(manufacturerOrigin).hostname,
  );
  const manufacturerHeaders = await (await authorizationRequest).allHeaders();
  expect(
    manufacturerHeaders.cookie?.includes("thermora_session=") ?? false,
  ).toBe(false);
  // The provider keeps its own appearance preference on its separate origin.
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-color-mode", "dark");
  await page.evaluate(() => document.fonts.ready);
  if (process.env.CAPTURE_UI === "1") {
    await page.screenshot({
      path: "../.impeccable/review/manufacturer-dark.png",
      fullPage: true,
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({
      path: "../.impeccable/review/manufacturer-mobile-dark.png",
      fullPage: true,
    });
    await page.setViewportSize({ width: 1440, height: 1000 });
  }
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  if (process.env.CAPTURE_UI === "1")
    await page.screenshot({
      path: "../.impeccable/review/manufacturer-light.png",
      fullPage: true,
    });
  await page.getByLabel("Manufacturer email").fill(access.manufacturer_login);
  await page
    .getByLabel("Manufacturer password")
    .fill(access.manufacturer_password);
  await page.getByRole("button", { name: "Authorize device access" }).click();
  await expect(
    page.getByRole("heading", { name: "Connect to Thermora Cloud" }),
  ).toBeVisible();
  const existing = await page.request.get(`/api/v1/devices/${seed.device_id}`);
  if (existing.status() === 404) {
    await page
      .getByRole("combobox", { name: "Heat pump", exact: true })
      .selectOption(seed.device_id);
    await page.getByLabel("Partner access code").fill("invalid-partner-code");
    await page.getByRole("button", { name: "Continue to building" }).click();
    await page.getByLabel("Building name").fill("Cloud browser test building");
    await page.getByLabel("Floor area (m²)").fill("120");
    await page.getByLabel("Street address").fill("Fictional street 1");
    await page.getByLabel("Postal code").fill("00000");
    await page.getByLabel("City", { exact: true }).fill("Demo city");
    await page.getByLabel("Electricity tariff").selectOption("variable");
    await page
      .getByLabel(
        "I accept the local simulation terms and authorize connecting this device.",
      )
      .check();
    await page.getByRole("button", { name: "Add to Thermora Cloud" }).click();
    await expect(page.getByRole("alert")).toContainText("partner access code");
    await page.getByRole("button", { name: "Back to equipment" }).click();
    await page
      .getByLabel("Partner access code")
      .fill(access.partner_access_code);
    await page.getByRole("button", { name: "Continue to building" }).click();
    await page.setViewportSize({ width: 390, height: 844 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBeTruthy();
    const keys: string[] = [];
    await page.route("**/api/v1/cloud-enrollments", async (route) => {
      keys.push(route.request().headers()["idempotency-key"]);
      const result = await route.fetch();
      if (result.status() === 201 && keys.length === 1) {
        await route.abort("connectionreset"); // Commit succeeds, but the browser loses the response.
      } else {
        await route.fulfill({ response: result });
      }
    });
    await page.getByRole("button", { name: "Add to Thermora Cloud" }).click();
    await expect(page.getByRole("alert")).toBeVisible();
    await page.getByRole("button", { name: "Add to Thermora Cloud" }).click();
    await expect(
      page.getByRole("heading", { name: "Operating timeline" }),
    ).toBeVisible();
    expect(keys).toHaveLength(2);
    expect(keys[0]).toBe(keys[1]);
  } else {
    expect(existing.ok()).toBeTruthy();
    const device = (await existing.json()) as components["schemas"]["Device"];
    await page.goto(`/buildings/${device.building_id}`);
  }
  await expect(
    page.getByRole("heading", { name: "Operating timeline" }),
  ).toBeVisible();
  const previousReports = await page.request.get(
    `/api/v1/devices/${seed.device_id}/comparisons`,
  );
  expect(previousReports.status()).toBe(200);
  const previous =
    (await previousReports.json()) as components["schemas"]["Comparison"][];
  if (previous.length > 0) {
    const restored = await page.request.get(
      `/api/v1/devices/${seed.device_id}/comparisons/${previous[0].id}/artifact`,
    );
    expect(restored.status()).toBe(200);
    expect(
      ((await restored.json()) as components["schemas"]["Comparison"]).plan,
    ).toEqual(previous[0].plan);
  }
  await expect(
    page.getByRole("button", { name: "Apply override" }),
  ).toBeEnabled({ timeout: 45_000 });
  const responsePromise = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/api/v1/devices/${seed.device_id}/overrides`) &&
      response.request().method() === "POST",
  );
  await page.getByLabel("Room target · °C", { exact: true }).fill("22");
  await page.getByLabel("Simulated minutes").selectOption("120");
  await page.getByRole("button", { name: "Apply override" }).click();
  const response = await responsePromise;
  expect(response.status()).toBe(202);
  const command = (await response.json()) as components["schemas"]["Command"];
  await expect
    .poll(
      async () => {
        const result = await page.request.get(
          `/api/v1/devices/${seed.device_id}/commands`,
        );
        const commands =
          (await result.json()) as components["schemas"]["Command"][];
        return commands.find((c) => c.id === command.id)?.stage;
      },
      { timeout: 30_000 },
    )
    .toBe("confirmed");
  const reading = await page.request.get(`/api/v1/devices/${seed.device_id}`);
  const device = (await reading.json()) as components["schemas"]["Device"];
  expect(device.latest?.control_mode).toBe("remote");
  expect(device.latest?.applied_command_seq).toBe(command.sequence);
  expect(errors).toEqual([]);
  expect(
    (
      await page.request.delete(`/api/v1/devices/${seed.device_id}/override`)
    ).status(),
  ).toBe(204);

  await page.getByRole("link", { name: "Schedule", exact: true }).click();
  await page
    .getByRole("combobox", { name: "Scenario", exact: true })
    .first()
    .selectOption("winter");
  await page.getByLabel("Enable price optimization").check();
  const optimizationStarted = Date.now();
  await page.getByRole("button", { name: "Apply scenario & control" }).click();
  const statePath = `/api/v1/devices/${seed.device_id}/optimization`;
  await expect
    .poll(
      async () => {
        const result = await page.request.get(statePath);
        const state =
          (await result.json()) as components["schemas"]["OptimizationState"];
        expect(state.integration_route).toBe("manufacturer");
        return state.enabled && !!state.plan;
      },
      { timeout: 45_000 },
    )
    .toBe(true);
  await expect
    .poll(
      async () => {
        const result = await page.request.get(
          `/api/v1/devices/${seed.device_id}/commands`,
        );
        const commands =
          (await result.json()) as components["schemas"]["Command"][];
        return commands.some(
          (c) =>
            Date.parse(c.issued_at_wall) >= optimizationStarted &&
            c.stage === "confirmed" &&
            [
              "normal",
              "cheap_before_peak",
              "expensive_interval",
              "fallback_constraints_or_cost",
            ].includes(c.reason_code),
        );
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  await page.getByRole("link", { name: "Statistics", exact: true }).click();
  const comparisonResponse = page.waitForResponse(
    (r) =>
      r.url().endsWith(`/api/v1/devices/${seed.device_id}/comparisons`) &&
      r.request().method() === "POST",
  );
  await page
    .getByRole("button", { name: "Compare controllers", exact: true })
    .click();
  const comparison = await comparisonResponse;
  expect(comparison.status()).toBe(201);
  const report =
    (await comparison.json()) as components["schemas"]["Comparison"];
  expect(report.plan.trace).toHaveLength(120);
  if (!report.plan.comparable) expect(report.plan.savings_eur).toBeNull();
  await page.getByRole("button", { name: "Save report to local S3" }).click();
  const link = page.getByRole("link", { name: "Download comparison JSON" });
  await expect(link).toBeVisible();
  const artifact = await page.request.get((await link.getAttribute("href"))!);
  expect(artifact.ok()).toBe(true);
  const downloaded =
    (await artifact.json()) as components["schemas"]["Comparison"];
  expect(downloaded.plan.input_hash).toBe(report.plan.input_hash);
  expect(downloaded.plan).toEqual(report.plan);

  // Pause only the fixture's process. MQTT/cloud stay healthy, so a bypass
  // would incorrectly keep cloud telemetry fresh. Always resume the container.
  const execute = promisify(execFile);
  const result = await execute(
    "docker",
    [
      "exec",
      "floci-eks-thermora-cloud",
      "kubectl",
      "-n",
      "thermora",
      "get",
      "pods",
      "-l",
      "app=cloud",
      "-o",
      'jsonpath={.items[0].status.containerStatuses[?(@.name=="manufacturer")].containerID}',
    ],
    { timeout: 15_000 },
  );
  const container = result.stdout.trim().replace(/^containerd:\/\//, "");
  expect(container).toMatch(/^[0-9a-f]{64}$/);
  const manufacturerTask = async (action: "pause" | "resume") =>
    execute(
      "docker",
      [
        "exec",
        "floci-eks-thermora-cloud",
        "ctr",
        "-n",
        "k8s.io",
        "tasks",
        action,
        container,
      ],
      { timeout: 15_000 },
    );
  await page.getByRole("link", { name: "Schedule", exact: true }).click();
  await manufacturerTask("pause");
  try {
    await expect
      .poll(
        async () => {
          const result = await page.request.get(
            `/api/v1/devices/${seed.device_id}`,
          );
          return ((await result.json()) as components["schemas"]["Device"])
            .connectivity;
        },
        { timeout: 35_000 },
      )
      .not.toBe("online");
    await page.getByLabel("Enable price optimization").uncheck();
    await page
      .getByRole("button", { name: "Apply scenario & control" })
      .click();
    await expect
      .poll(async () => {
        const result = await page.request.get(statePath);
        return (
          (await result.json()) as components["schemas"]["OptimizationState"]
        ).enabled;
      })
      .toBe(false);
  } finally {
    await manufacturerTask("resume");
  }
  await expect
    .poll(
      async () => {
        const result = await page.request.get(
          `/api/v1/devices/${seed.device_id}`,
        );
        const current =
          (await result.json()) as components["schemas"]["Device"];
        return (
          current.connectivity === "online" &&
          current.desired_version === current.reported_version &&
          current.latest?.control_mode === "baseline"
        );
      },
      { timeout: 45_000 },
    )
    .toBe(true);
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  const runPath = `/api/v1/devices/${seed.device_id}/run`;
  const beforeRun = (await (
    await page.request.get(runPath)
  ).json()) as components["schemas"]["RunState"];
  await page
    .getByRole("button", { name: "Pause simulation", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Resume simulation", exact: true }),
  ).toBeEnabled({ timeout: 30_000 });
  const pausedReading = (await (
    await page.request.get(`/api/v1/devices/${seed.device_id}`)
  ).json()) as components["schemas"]["Device"];
  await expect
    .poll(async () => {
      const current = (await (
        await page.request.get(`/api/v1/devices/${seed.device_id}`)
      ).json()) as components["schemas"]["Device"];
      expect(current.latest?.sim_elapsed_seconds).toBe(
        pausedReading.latest?.sim_elapsed_seconds,
      );
      return current.connectivity;
    })
    .toBe("online");
  await page
    .getByRole("button", { name: "Resume simulation", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Pause simulation", exact: true }),
  ).toBeEnabled({ timeout: 30_000 });
  await page.getByText("Start a new run", { exact: true }).click();
  await page
    .getByRole("combobox", { name: "Equipment model", exact: true })
    .selectOption("heating-dhw");
  await page.getByLabel("I want to reset this simulated heat pump.").check();
  await page
    .getByRole("button", { name: "Reset simulation", exact: true })
    .click();
  await expect
    .poll(
      async () => {
        const run = (await (
          await page.request.get(runPath)
        ).json()) as components["schemas"]["RunState"];
        return run.epoch === beforeRun.epoch + 1 && run.status === "ready";
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  await expect
    .poll(
      async () => {
        const current = (await (
          await page.request.get(`/api/v1/devices/${seed.device_id}`)
        ).json()) as components["schemas"]["Device"];
        return (
          current.run_id !== beforeRun.run_id &&
          current.connectivity === "online" &&
          current.desired_version === current.reported_version
        );
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  await expect(page.getByLabel("Start heating at · °C")).toBeVisible();
  await page.getByLabel("Start heating at · °C").fill("50");
  await page.getByLabel("Stop heating at · °C").fill("55");
  await page
    .getByRole("button", { name: "Save device preferences", exact: true })
    .click();
  await expect
    .poll(
      async () => {
        const current = (await (
          await page.request.get(`/api/v1/devices/${seed.device_id}`)
        ).json()) as components["schemas"]["Device"];
        return (
          current.latest?.hot_water?.start_c === 50 &&
          current.latest.hot_water.stop_c === 55 &&
          current.desired_version === current.reported_version
        );
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  await page.getByRole("link", { name: "Statistics", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "History & statistics" }),
  ).toBeVisible();
  await page
    .getByRole("combobox", { name: "Simulation run", exact: true })
    .selectOption(beforeRun.run_id);
  await expect(
    page.locator("dt").filter({ hasText: /^Covered energy$/ }),
  ).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  expect(errors).toEqual([]);
  await page.screenshot({
    path: "test-results/cloud-control-mobile.png",
    fullPage: true,
  });
});
