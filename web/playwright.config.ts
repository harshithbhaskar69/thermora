import { readFileSync } from "node:fs";
import { defineConfig } from "@playwright/test";

const origins =
  process.env.THERMORA_CLOUD_TEST === "1"
    ? (JSON.parse(
        readFileSync(
          new URL("../.local/cloud/public-origins.json", import.meta.url),
          "utf8",
        ),
      ) as { cloud: string; manufacturer: string })
    : {
        cloud: "https://localhost:8443",
        manufacturer: "https://manufacturer.localhost:8444",
      };

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  use: {
    channel: process.env.PLAYWRIGHT_CHANNEL,
    baseURL: origins.cloud,
    ignoreHTTPSErrors: true,
    viewport: { width: 1440, height: 1000 },
    // Login and claim requests contain local secrets: do not capture network traces.
    trace: "off",
    screenshot: "only-on-failure",
  },
});
