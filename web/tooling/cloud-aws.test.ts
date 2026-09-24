import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const script = fileURLToPath(
  new URL("../../scripts/cloud-aws.sh", import.meta.url),
);

test("Floci CLI isolates credentials and pins the local endpoint while preserving arguments", () => {
  const root = mkdtempSync(join(tmpdir(), "thermora-aws-test-"));
  try {
    writeFileSync(
      join(root, "aws"),
      `#!/bin/sh
printf '%s\\n' "$AWS_CONFIG_FILE" "$AWS_SHARED_CREDENTIALS_FILE" \
  "\${AWS_ACCESS_KEY_ID-unset}" "\${AWS_SECRET_ACCESS_KEY-unset}" \
  "\${AWS_SESSION_TOKEN-unset}" "\${AWS_SECURITY_TOKEN-unset}" \
  "\${AWS_REGION-unset}" "\${AWS_DEFAULT_REGION-unset}" \
  "$AWS_EC2_METADATA_DISABLED" "$AWS_CLI_AUTO_PROMPT" "$AWS_PAGER" \
  "$NO_PROXY" "$no_proxy" "$@"
exit 7
`,
      { mode: 0o700 },
    );
    const args = [
      "eks",
      "list-clusters",
      "--query",
      "clusters | [0]",
      "--output",
      "text",
      "--profile",
      "other",
      "--endpoint-url",
      "https://example.invalid",
    ];
    const result = spawnSync("/bin/sh", [script, ...args], {
      cwd: root,
      encoding: "utf8",
      timeout: 5000,
      env: {
        PATH: `${root}:/usr/bin:/bin`,
        AWS_CONFIG_FILE: "/unused/config",
        AWS_SHARED_CREDENTIALS_FILE: "/unused/credentials",
        AWS_ACCESS_KEY_ID: "ambient-key",
        AWS_SECRET_ACCESS_KEY: "ambient-secret",
        AWS_SESSION_TOKEN: "ambient-session",
        AWS_SECURITY_TOKEN: "ambient-security",
        AWS_REGION: "eu-west-1",
        AWS_DEFAULT_REGION: "eu-west-1",
        AWS_PROFILE: "other",
        AWS_ENDPOINT_URL: "https://example.invalid",
        AWS_ENDPOINT_URL_EKS: "https://example.invalid",
        AWS_IGNORE_CONFIGURED_ENDPOINT_URLS: "true",
        AWS_CLI_AUTO_PROMPT: "on",
        AWS_PAGER: "unwanted-pager",
        NO_PROXY: "example.invalid",
        no_proxy: "example.invalid",
      },
    });
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(7);
    expect(result.stderr).toBe("");
    expect(result.stdout.trimEnd().split("\n")).toEqual([
      join(dirname(script), "../deploy/cloud/aws.config"),
      "/dev/null",
      "unset",
      "unset",
      "unset",
      "unset",
      "unset",
      "unset",
      "true",
      "off",
      "",
      "127.0.0.1",
      "127.0.0.1",
      ...args,
      "--profile",
      "floci",
      "--endpoint-url",
      "http://127.0.0.1:4566",
    ]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
