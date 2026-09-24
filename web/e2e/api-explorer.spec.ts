import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

test("API explorer works locally and uses normal session authorization", async ({
  page,
  baseURL,
}, testInfo) => {
  const externalRequests: string[] = [];
  const browserErrors: string[] = [];
  page.on("pageerror", (error) => browserErrors.push(error.message));
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== baseURL) {
      externalRequests.push(url.origin);
      await route.abort();
      return;
    }
    await route.continue();
  });
  // Query parameters cannot replace the local contract with a remote document.
  await page.goto("/docs/?url=https://example.invalid/spec.json");
  await expect(
    page.getByRole("heading", { name: "API explorer", exact: true }),
  ).toBeVisible();
  for (const tag of ["Session", "Buildings", "Devices", "Edge", "Health"]) {
    await expect(page.locator(`.opblock-tag[data-tag="${tag}"]`)).toBeVisible();
  }
  const specResponse = await page.request.get("/openapi.yaml");
  expect(specResponse.status()).toBe(200);
  const spec = await specResponse.text();
  expect(await page.locator(".opblock").count()).toBe(
    [...spec.matchAll(/^\s+operationId:/gm)].length,
  );

  const health = page.locator("#operations-Health-getLiveness");
  await health.locator(".opblock-summary-control").click();
  await health.getByRole("button", { name: "Try it out" }).click();
  const healthResponse = page.waitForResponse("**/health/live");
  await health.getByRole("button", { name: "Execute", exact: true }).click();
  expect((await healthResponse).status()).toBe(204);
  await expect(
    health
      .locator(".live-responses-table")
      .getByRole("cell", { name: "204", exact: true }),
  ).toBeVisible();

  const session = page.locator("#operations-Session-getSession");
  await session.locator(".opblock-summary-control").click();
  await session.getByRole("button", { name: "Try it out" }).click();
  const unauthorized = page.waitForResponse("**/api/v1/session");
  await session.getByRole("button", { name: "Execute", exact: true }).click();
  expect((await unauthorized).status()).toBe(401);

  const access = JSON.parse(
    await readFile(
      new URL(
        process.env.THERMORA_CLOUD_TEST === "1"
          ? "../../.local/cloud/devices/e2e/browser-account.json"
          : "../../.local/demo-access.json",
        import.meta.url,
      ),
      "utf8",
    ),
  ) as { login: string; password: string };
  // APIRequestContext shares the browser's cookie jar without exposing secrets in the UI.
  const login = await page.request.post("/api/v1/session", {
    data: { login: access.login, password: access.password },
  });
  expect(login.status()).toBe(200);
  const authorized = page.waitForResponse("**/api/v1/session");
  await session.getByRole("button", { name: "Execute", exact: true }).click();
  expect((await authorized).status()).toBe(200);
  await expect(
    session
      .locator(".live-responses-table")
      .getByRole("cell", { name: "200", exact: true }),
  ).toBeVisible();

  await health.locator(".opblock-summary-control").click();
  await session.locator(".opblock-summary-control").click();
  await page.screenshot({
    path: testInfo.outputPath("api-explorer.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBeTruthy();
  await page.screenshot({
    path: testInfo.outputPath("api-explorer-mobile.png"),
    fullPage: true,
  });
  expect(externalRequests).toEqual([]);
  expect(browserErrors).toEqual([]);
  await page.getByRole("link", { name: "Manufacturer API reference" }).click();
  await expect(page.locator(".info .title")).toContainText(
    "Thermora simulated manufacturer API",
  );
  expect(await page.getByRole("button", { name: "Try it out" }).count()).toBe(
    0,
  );
  expect(await page.locator(".opblock").count()).toBe(4);
});
