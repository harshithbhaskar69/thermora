import { test, expect, type Page } from "@playwright/test";
import { installAPI, buildingID, deviceID, secondID } from "./fixtures";

const buildingURL = `/buildings/${buildingID}?device=${deviceID}`;
async function noOverflow(page: Page) {
  await expect
    .poll(() =>
      page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
    )
    .toBe(true);
}
async function capture(page: Page, name: string) {
  await expect(page.getByText("Loading charts…", { exact: true })).toBeHidden();
  await page.evaluate(async () => {
    await document.fonts.ready;
    await Promise.allSettled(document.getAnimations().map((a) => a.finished));
    window.scrollTo(0, 0);
    await new Promise<void>((resolve) =>
      requestAnimationFrame(() => resolve()),
    );
  });
  if (process.env.CAPTURE_UI === "1")
    await page.screenshot({
      path: `../.impeccable/review/${name}.png`,
      fullPage: true,
    });
}

test("themes follow system, persist explicit choice and synchronize tabs", async ({
  page,
  context,
}) => {
  await installAPI(page);
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto(buildingURL);
  await expect(page.locator("html")).toHaveAttribute("data-color-mode", "dark");
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("html")).toHaveAttribute(
    "data-color-mode",
    "light",
  );
  await page.emulateMedia({ colorScheme: "dark" });
  await page.getByRole("button", { name: "Switch to light theme" }).focus();
  await page.keyboard.press("Enter");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute(
    "data-color-mode",
    "light",
  );
  const other = await context.newPage();
  await installAPI(other);
  await other.goto(buildingURL);
  await other.getByRole("button", { name: "Switch to dark theme" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-color-mode", "dark");
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("html")).toHaveAttribute("data-color-mode", "dark");
  await other.evaluate(() => localStorage.removeItem("thermora-theme"));
  await expect(page.locator("html")).toHaveAttribute("data-theme", "system");
  await expect(page.locator("html")).toHaveAttribute(
    "data-color-mode",
    "light",
  );
  await other.close();
});

test("sun and moon control stays in the top-right corner on login and workspace", async ({
  page,
}) => {
  const state = await installAPI(page);
  for (const signedIn of [false, true]) {
    state.signedIn = signedIn;
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto(signedIn ? buildingURL : "/");
      const toggle = page.getByRole("button", {
        name: /Switch to (light|dark) theme/,
      });
      await expect(toggle).toBeVisible();
      const box = await toggle.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.width).toBe(44);
      expect(box!.height).toBe(44);
      expect(box!.y).toBeLessThanOrEqual(24);
      expect(width - box!.x - box!.width).toBeGreaterThanOrEqual(16);
      expect(width - box!.x - box!.width).toBeLessThanOrEqual(32);
      await expect(page.getByLabel("Appearance", { exact: true })).toHaveCount(
        0,
      );
      await expect(toggle.locator(".theme-moon")).toBeVisible();
      await toggle.locator("svg").click();
      await expect(toggle).toHaveAccessibleName("Switch to light theme");
      await expect(toggle.locator(".theme-sun")).toBeVisible();
      await expect(toggle.locator(".theme-moon")).toBeHidden();
      await noOverflow(page);
      await capture(page, `theme-${signedIn ? "workspace" : "login"}-${width}`);
      await toggle.focus();
      await page.keyboard.press("Space");
      await expect(toggle).toHaveAccessibleName("Switch to dark theme");
    }
  }
});

test("unavailable storage still permits theme selection", async ({ page }) => {
  await page.addInitScript(() =>
    Object.defineProperty(window, "localStorage", {
      get() {
        throw new DOMException("Blocked", "SecurityError");
      },
    }),
  );
  await installAPI(page);
  await page.goto(buildingURL);
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-color-mode", "dark");
});

test("zone and view survive deep links and back navigation without querying foreign equipment", async ({
  page,
}) => {
  const state = await installAPI(page);
  await page.goto(buildingURL);
  await expect(
    page
      .getByRole("navigation", { name: "Workspace", exact: true })
      .getByRole("link", { name: "Buildings", exact: true }),
  ).toHaveAttribute("aria-current", "location");
  await page.getByLabel("Heating zone").selectOption(secondID);
  await page.getByRole("link", { name: "Schedule", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`view=schedule&device=${secondID}`));
  await page.reload();
  await expect(page.getByLabel("Heating zone")).toHaveValue(secondID);
  await expect(
    page
      .getByRole("navigation", { name: "Workspace", exact: true })
      .getByRole("link", { name: "Buildings", exact: true }),
  ).toHaveAttribute("aria-current", "location");
  await page.goBack();
  await expect(
    page.getByRole("heading", { name: "Operating timeline" }),
  ).toBeVisible();
  await page.goto(`/buildings/${buildingID}?device=unowned-zone`);
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "unavailable in this building" }),
  ).toBeVisible();
  expect(state.requests.some((r) => r.includes("/devices/unowned-zone"))).toBe(
    false,
  );
});

test("chart inspection stays fixed while data polls and remains keyboard controllable", async ({
  page,
}) => {
  const state = await installAPI(page);
  await page.goto(buildingURL);
  const chart = page.locator(".chart svg");
  await expect(chart).toBeVisible();
  await page.getByRole("button", { name: "Zoom in", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText(/Inspecting a fixed window/)).toBeVisible();
  const before = await chart.innerHTML();
  state.series = state.series.map((s) => ({
    ...s,
    indoor_temperature_c: 25,
    electrical_power_w: 5000,
  }));
  await page.waitForResponse((r) => r.url().endsWith("/series"));
  expect(await chart.innerHTML()).toBe(before);
  await page.getByRole("button", { name: "Return to live" }).click();
  await expect.poll(() => chart.innerHTML()).not.toBe(before);
  await page
    .getByRole("checkbox", { name: "Electrical power", exact: true })
    .uncheck();
  await page.waitForResponse((r) => r.url().endsWith("/series"));
  await expect(
    page.getByRole("checkbox", { name: "Electrical power", exact: true }),
  ).not.toBeChecked();
  await page.getByRole("button", { name: "Zoom in", exact: true }).click();
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expect(page.getByText(/Inspecting a fixed window/)).toBeVisible();
});

test("stale and absent telemetry and rejected commands remain explicit", async ({
  page,
}) => {
  const state = await installAPI(page);
  state.building.devices[0].connectivity = "offline";
  await page.goto(buildingURL);
  await expect(
    page.getByRole("button", { name: "Apply override", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("status").filter({ hasText: "Telemetry is stale" }),
  ).toBeVisible();
  await capture(page, "offline");
  state.building.devices[0].connectivity = "online";
  state.overrideRejected = true;
  await expect(
    page.getByRole("button", { name: "Apply override", exact: true }),
  ).toBeEnabled();
  await page
    .getByRole("button", { name: "Apply override", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText("Telemetry is stale");
  await expect(
    page.getByText("Override recorded.", { exact: false }),
  ).toHaveCount(0);
  const observations = state.series;
  state.series = [];
  delete state.building.devices[0].latest;
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "No telemetry yet" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Apply override", exact: true }),
  ).toBeDisabled();
  await capture(page, "empty");
  state.series = observations;
  state.building.devices[0].latest = observations.at(-1);
  await expect(
    page.getByRole("button", { name: "Apply override", exact: true }),
  ).toBeEnabled();
  state.building.devices[0].control_connected = false;
  await expect(
    page.getByRole("button", { name: "Apply override", exact: true }),
  ).toBeDisabled();
  state.denied = true;
  await page.reload();
  await expect(page.getByRole("alert")).toContainText("You do not have access");
  await expect(
    page.getByRole("button", { name: "Apply override", exact: true }),
  ).toHaveCount(0);
});

test("status shows acknowledged pacing and pending run changes", async ({
  page,
}) => {
  const state = await installAPI(page);
  await page.goto(buildingURL);
  const status = page.locator(".status-strip");
  await expect(status).toContainText("Running · 60×");
  state.run.paused = true;
  state.run.rate = 10;
  await expect(status).toContainText("Paused · 10×");
  state.run.status = "pending";
  state.run.action = "resume";
  await expect(status).toContainText("Run change pending: resume");
  await expect(status).not.toContainText("Running");
  state.run.status = "ready";
  state.run.paused = false;
  await expect(status).toContainText("Running · 10×");
});

test("schedule and statistics preserve negative prices, missing data and repeated DST hours", async ({
  page,
}) => {
  await installAPI(page);
  await page.goto(buildingURL + "&view=schedule");
  await expect(page.locator(".chart svg")).toBeVisible();
  await page.getByText("All 96 schedule intervals", { exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "-0.05", exact: true }).first(),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "Missing", exact: true }).first(),
  ).toBeVisible();
  await expect(page.getByRole("cell", { name: /03:00 GMT\+3/ })).toBeVisible();
  await expect(page.getByRole("cell", { name: /03:00 GMT\+2/ })).toBeVisible();
  await page.getByText("All 96 schedule intervals", { exact: true }).click();
  await capture(page, "schedule");
  await page.getByRole("link", { name: "Statistics", exact: true }).click();
  await expect(page.locator(".chart svg")).toHaveCount(3);
  await expect(
    page.getByText("A complete window cost is unavailable.", { exact: false }),
  ).toBeVisible();
  const historyResponse = page.waitForResponse(
    (r) => r.url().includes("/history?") && r.url().includes("from="),
  );
  await page
    .getByRole("combobox", { name: "Window", exact: true })
    .selectOption("week");
  await historyResponse;
  await page.getByText("Hourly values and coverage", { exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "—", exact: true }).first(),
  ).toBeVisible();
  await page.getByText("Hourly values and coverage", { exact: true }).click();
  await capture(page, "statistics");
});

test("onboarding validates each step and keeps input after server rejection", async ({
  page,
}) => {
  await installAPI(page);
  await page.goto("/connect");
  await page.getByRole("button", { name: "Continue to building" }).click();
  await expect(page.getByLabel("Partner access code")).toBeVisible();
  await expect(page.getByLabel("Building name", { exact: true })).toBeHidden();
  await page.getByLabel("Partner access code").fill("synthetic-test-code");
  await page.getByRole("button", { name: "Continue to building" }).click();
  await page.getByLabel("Building name", { exact: true }).fill("Test building");
  await page.getByLabel("Floor area (m²)").fill("100");
  await page.getByLabel("Street address").fill("Fictional street");
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
  await expect(page.getByLabel("Partner access code")).toHaveValue(
    "synthetic-test-code",
  );
  await page.getByRole("button", { name: "Continue to building" }).click();
  await expect(page.getByLabel("Building name", { exact: true })).toHaveValue(
    "Test building",
  );
  await capture(page, "onboarding");
});

test("settings retain unsaved edits on conflict and fleet filters retain real device links", async ({
  page,
}) => {
  const state = await installAPI(page);
  state.profileConflict = true;
  await page.goto(buildingURL + "&view=settings");
  await page
    .getByLabel("Building name", { exact: true })
    .fill("My updated building");
  await page
    .getByRole("button", { name: "Save building", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText("another session");
  await expect(page.getByLabel("Building name", { exact: true })).toHaveValue(
    "My updated building",
  );
  await capture(page, "settings");
  await page.goto("/fleet");
  await page
    .getByRole("combobox", { name: "Availability", exact: true })
    .selectOption("excluded");
  const equipment = page.locator("section").filter({
    has: page.getByRole("heading", { name: "Equipment availability" }),
  });
  await expect(equipment.getByRole("link")).toHaveCount(1);
  await expect(equipment.getByRole("link")).toHaveAttribute(
    "href",
    `/buildings/${buildingID}?device=${secondID}`,
  );
  await page
    .getByRole("combobox", { name: "Availability", exact: true })
    .selectOption("all");
  await page.getByLabel("Find building or zone").fill("missing");
  await expect(
    page.getByRole("status").filter({ hasText: "No equipment matches" }),
  ).toBeVisible();
  await page.getByLabel("Find building or zone").fill("");
  await capture(page, "fleet");
});

for (const mode of ["light", "dark"] as const) {
  test(`${mode} workspace adapts at desktop, tablet and small mobile widths`, async ({
    page,
  }) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await installAPI(page);
    await page.emulateMedia({ colorScheme: mode, reducedMotion: "reduce" });
    await page.goto(buildingURL);
    await expect(page.locator(".chart svg")).toBeVisible();
    for (const width of [1536, 1440, 1024, 390, 320]) {
      await page.setViewportSize({ width, height: width < 700 ? 844 : 1024 });
      await noOverflow(page);
      await expect(
        page.getByText("Comfort range", { exact: true }).first(),
      ).toBeVisible();
      await expect
        .poll(() =>
          page
            .locator(".recent-activity .table-scroll")
            .evaluate((element) => element.scrollWidth <= element.clientWidth),
        )
        .toBe(true);
      await expect
        .poll(() =>
          page.locator(".chart svg").evaluate((svg) => {
            const bounds = svg.getBoundingClientRect();
            return [...svg.querySelectorAll("text")].every((text) => {
              const box = text.getBoundingClientRect();
              return (
                box.left >= bounds.left - 1 && box.right <= bounds.right + 1
              );
            });
          }),
        )
        .toBe(true);
      await capture(
        page,
        width === 1440 && mode === "light"
          ? "desktop"
          : width === 390 && mode === "light"
            ? "mobile"
            : `status-${mode}-${width}`,
      );
      if (width === 1536 && mode === "light" && process.env.CAPTURE_UI === "1")
        await page.screenshot({ path: "../.impeccable/review/hero-repro.png" });
    }
    await page.getByRole("button", { name: "Workspace menu" }).click();
    await expect(
      page.getByRole("link", { name: "Fleet operations", exact: true }),
    ).toBeVisible();
    await page.getByRole("link", { name: "Activity", exact: true }).click();
    await expect(page.locator(".command").first()).toContainText(
      "Telemetry confirmed",
    );
    await capture(page, `activity-${mode}`);
    await page.getByRole("link", { name: "Settings", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "Building settings" }),
    ).toBeVisible();
    await noOverflow(page);
    expect(errors).toEqual([]);
  });
}

test("login, overview and API explorer share the theme without third-party asset requests", async ({
  page,
}) => {
  const state = await installAPI(page);
  state.signedIn = false;
  const external: string[] = [];
  page.on("request", (request) => {
    if (!request.url().startsWith("http://127.0.0.1:4173/"))
      external.push(request.url());
  });
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Sign in to Thermora" }),
  ).toBeVisible();
  await capture(page, "login");
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await page.setViewportSize({ width: 390, height: 844 });
  await noOverflow(page);
  await capture(page, "login-mobile-dark");
  state.signedIn = true;
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Buildings", exact: true }),
  ).toBeVisible();
  await capture(page, "overview-mobile-dark");
  await page.goto("/docs/");
  await expect(page.locator("html")).toHaveAttribute("data-color-mode", "dark");
  await expect(
    page.getByRole("heading", { name: "API explorer" }),
  ).toBeVisible();
  await expect(page.locator('.opblock-tag[data-tag="Health"]')).toBeVisible();
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  await expect(page.locator("html")).toHaveAttribute(
    "data-color-mode",
    "light",
  );
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await noOverflow(page);
  await capture(page, "explorer-mobile-dark");
  expect(external).toEqual([]);
});
