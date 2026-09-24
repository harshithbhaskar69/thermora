import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execPath } from "node:process";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { afterEach, expect, test } from "vitest";

const seedScript = fileURLToPath(
  new URL("../../scripts/seed-demo.sh", import.meta.url),
);
const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

test.each([
  { exitCode: 0, device: "" },
  { exitCode: 1, device: "" },
  { exitCode: 0, device: "helsinki" },
  { exitCode: 1, device: "helsinki" },
])(
  "demo seed is atomic and private: $device, exit $exitCode",
  ({ exitCode, device }) => {
    const root = mkdtempSync(join(tmpdir(), "thermora-seed-test-"));
    roots.push(root);
    const directory = device
      ? join(root, ".local/devices", device)
      : join(root, ".local");
    mkdirSync(directory, { recursive: true });
    const output = join(directory, "demo-access.json");
    writeFileSync(output, "previous access", { mode: 0o600 });

    // Replace Compose with a process that writes synthetic output, then succeeds or fails.
    const result = spawnSync(
      "sh",
      [
        seedScript,
        execPath,
        "-e",
        `process.stdout.write('replacement access'); process.exit(${String(exitCode)})`,
      ],
      {
        cwd: root,
        encoding: "utf8",
        timeout: 5000,
        env: { ...process.env, THERMORA_DEVICE: device },
      },
    );

    expect(result.error).toBeUndefined();
    expect(result.status).toBe(exitCode);
    expect(readFileSync(output, "utf8")).toBe(
      exitCode === 0 ? "replacement access" : "previous access",
    );
    expect(statSync(output).mode & 0o777).toBe(0o600);
    expect(readdirSync(directory)).toEqual(["demo-access.json"]);
  },
);
