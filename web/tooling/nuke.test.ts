import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execPath } from "node:process";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { afterEach, expect, test } from "vitest";

const script = fileURLToPath(new URL("../../scripts/nuke.sh", import.meta.url));
const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true });
});

function fixture() {
  const root = mkdtempSync(join(tmpdir(), "thermora-nuke-test-"));
  roots.push(root);
  for (const dir of ["scripts", "tools", ".local/cloud/pki", ".task"])
    mkdirSync(join(root, dir), { recursive: true });
  copyFileSync(script, join(root, "scripts/nuke.sh"));
  writeFileSync(join(root, ".local/cloud/pki/ca.pem"), "synthetic certificate");
  writeFileSync(join(root, ".local/keep-until-success"), "private fixture");
  writeFileSync(join(root, "source.txt"), "keep source");

  // No real Docker, sudo or Task invocation can escape this disposable checkout.
  const mock = `#!${execPath}
const fs = require('node:fs');
const path = require('node:path');
const tool = path.basename(process.argv[1]);
const args = process.argv.slice(2);
const command = tool + ' ' + args.join(' ');
fs.appendFileSync('calls.log', command + '\\n');
if (process.env.FAIL_COMMAND && command.startsWith(process.env.FAIL_COMMAND)) process.exit(9);
if (tool === 'task') {
  if (args.join(' ') !== 'cloud:local-uninstall' || !fs.existsSync('.local/cloud/pki/ca.pem')) process.exit(8);
} else if (tool === 'grep') {
  process.exit(process.env.STALE_HOSTS === '1' ? 0 : 1);
} else if (tool === 'docker') {
  const selection = args.find(arg => arg.startsWith('label=com.docker.compose.project='));
  const project = selection?.split('=').at(-1);
  const owned = project === 'thermora' || project === 'thermora-cloud';
  const empty = process.env.EMPTY_DOCKER === '1';
  let output = '';
  if (args[0] === 'context') output = process.env.CONTEXT_ENDPOINT || 'unix:///synthetic/docker.sock';
  else if (args[0] === 'info') output = 'synthetic daemon';
  else if (args[0] === 'container' && args[1] === 'ls') {
    if (owned && !empty) output = project + '-container';
    else if (args.includes('ancestor=shared-image') && !empty) output = 'unrelated-container';
    else if (args.includes('name=^/floci-eks-thermora-cloud$') &&
      args.includes('label=io.floci.service=eks') &&
      args.includes('label=io.floci.resource-id=thermora-cloud') && !empty) output = 'local-node';
    else if (!owned && !args.some(arg => arg.startsWith('ancestor=')) &&
      !args.includes('name=^/floci-eks-thermora-cloud$')) process.exit(8);
  } else if (args[1] === 'ls') {
    if (args[0] === 'volume' && args.includes('name=^floci-eks-thermora-cloud$')) {
      if (!empty) output = 'floci-eks-thermora-cloud';
    } else if (!owned) process.exit(8);
    else if (!empty) output = args[0] === 'image'
      ? (project === 'thermora' ? 'shared-image' : 'owned-image')
      : project + '-' + args[0];
  } else if (args[1] === 'rm') {
    if (args.some(arg => arg.includes('unrelated') || arg === 'shared-image')) process.exit(8);
  } else process.exit(8);
  if (output) process.stdout.write(output + '\\n');
} else process.exit(8);
`;
  for (const tool of ["docker", "task", "grep"])
    writeFileSync(join(root, "tools", tool), mock, { mode: 0o700 });

  return {
    root,
    run(input: string, env: Record<string, string | undefined> = {}) {
      return spawnSync("/bin/sh", [join(root, "scripts/nuke.sh")], {
        cwd: tmpdir(), // Cleanup must use the script's checkout, never the caller's cwd.
        input,
        encoding: "utf8",
        timeout: 10000,
        env: { PATH: `${join(root, "tools")}:/usr/bin:/bin`, ...env },
      });
    },
    calls() {
      const file = join(root, "calls.log");
      return existsSync(file)
        ? readFileSync(file, "utf8").trimEnd().split("\n")
        : [];
    },
  };
}

test.each(["n\n", "\n", "", "anything\n"])(
  "nuke cancels without touching Docker, trust or data: %j",
  (answer) => {
    const f = fixture();
    const result = f.run(answer);
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(0);
    expect(result.stdout).toContain("Cancelled");
    expect(f.calls()).toEqual([]);
    expect(existsSync(join(f.root, ".local/keep-until-success"))).toBe(true);
  },
);

test("nuke removes both profiles and the node before local state, preserving shared resources", () => {
  const f = fixture();
  const result = f.run("y\n");
  expect(result.error).toBeUndefined();
  expect(result.stderr).toBe("");
  expect(result.status).toBe(0);
  const calls = f.calls();
  expect(calls).toContain("task cloud:local-uninstall");
  expect(calls).toContain(
    "docker container rm --force --volumes thermora-cloud-container",
  );
  expect(calls).toContain(
    "docker container rm --force --volumes thermora-container",
  );
  expect(calls).toContain("docker container rm --force --volumes local-node");
  expect(calls).toContain("docker volume rm floci-eks-thermora-cloud");
  expect(calls).toContain("docker volume rm thermora-cloud-volume");
  expect(calls).toContain("docker volume rm thermora-volume");
  expect(calls).toContain("docker network rm thermora-cloud-network");
  expect(calls).toContain("docker network rm thermora-network");
  expect(calls).toContain("docker image rm owned-image");
  expect(
    calls.findIndex((c) => c.startsWith("docker container rm")),
  ).toBeGreaterThan(calls.indexOf("task cloud:local-uninstall"));
  expect(result.stdout).toContain("Keeping shared image");
  expect(existsSync(join(f.root, ".local"))).toBe(false);
  expect(existsSync(join(f.root, ".task"))).toBe(false);
  expect(readFileSync(join(f.root, "source.txt"), "utf8")).toBe("keep source");
});

test.each([
  "task cloud:local-uninstall",
  "docker info",
  "docker volume rm",
  "docker image rm",
])("nuke preserves local credentials after a failed cleanup: %s", (command) => {
  const f = fixture();
  const result = f.run("yes\n", { FAIL_COMMAND: command });
  expect(result.error).toBeUndefined();
  expect(result.status).toBe(9);
  expect(existsSync(join(f.root, ".local/cloud/pki/ca.pem"))).toBe(true);
  expect(existsSync(join(f.root, ".local/keep-until-success"))).toBe(true);
  if (command === "task cloud:local-uninstall" || command === "docker info")
    expect(f.calls().some((c) => c.includes(" rm "))).toBe(false);
});

test.each([
  { DOCKER_HOST: "ssh://remote.invalid" },
  { DOCKER_CONTEXT: "remote", CONTEXT_ENDPOINT: "tcp://remote.invalid:2376" },
])("nuke refuses a remote Docker daemon: %j", (env) => {
  const f = fixture();
  const result = f.run("y\n", env);
  expect(result.status).toBe(1);
  expect(result.stderr).toContain("local Docker context");
  expect(
    f
      .calls()
      .some((c) => c === "task cloud:local-uninstall" || c.includes(" rm ")),
  ).toBe(false);
  expect(existsSync(join(f.root, ".local"))).toBe(true);
});

test("nuke handles already-removed Docker resources", () => {
  const f = fixture();
  const result = f.run("Y\n", { EMPTY_DOCKER: "1" });
  expect(result.status).toBe(0);
  expect(f.calls().some((c) => c.includes(" rm "))).toBe(false);
  expect(existsSync(join(f.root, ".local"))).toBe(false);
  const retry = f.run("y\n", { EMPTY_DOCKER: "1" });
  expect(retry.status).toBe(0);
  expect(
    f.calls().filter((c) => c === "task cloud:local-uninstall"),
  ).toHaveLength(1);
});

test("nuke keeps state when host entries remain but the uninstall CA is missing", () => {
  const f = fixture();
  rmSync(join(f.root, ".local/cloud/pki/ca.pem"));
  const result = f.run("y\n", { STALE_HOSTS: "1" });
  expect(result.status).toBe(1);
  expect(result.stderr).toContain("Local CA is missing");
  expect(f.calls().some((c) => c.includes(" rm "))).toBe(false);
  expect(existsSync(join(f.root, ".local/keep-until-success"))).toBe(true);
});

test("nuke refuses symlinked installation state", () => {
  const f = fixture();
  const other = mkdtempSync(join(tmpdir(), "thermora-other-installation-"));
  roots.push(other);
  writeFileSync(join(other, "keep"), "other installation");
  rmSync(join(f.root, ".local"), { recursive: true });
  symlinkSync(other, join(f.root, ".local"));
  const result = f.run("y\n");
  expect(result.status).toBe(1);
  expect(result.stderr).toContain("another installation");
  expect(f.calls().some((c) => c.includes(" rm "))).toBe(false);
  expect(existsSync(join(other, "keep"))).toBe(true);
});
