import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execPath } from "node:process";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

test.each(["restoring", "stopped", "unavailable"])(
  "cloud start handles cluster state %s without provisioning or pulling images",
  (state) => {
    const root = mkdtempSync(join(tmpdir(), "thermora-cloud-start-"));
    try {
      for (const dir of ["scripts", "tools"]) mkdirSync(join(root, dir));
      for (const name of ["cloud.sh", "cloud-aws.sh"])
        copyFileSync(
          fileURLToPath(new URL(`../../scripts/${name}`, import.meta.url)),
          join(root, "scripts", name),
        );
      // Exercise the real shared restart script without touching a Docker daemon.
      const mock = `#!${execPath}
const fs = require('node:fs');
const path = require('node:path');
const tool = path.basename(process.argv[1]);
const args = process.argv.slice(2);
fs.appendFileSync('calls.log', tool + ' ' + args.join(' ') + '\\n');
if (tool === 'sleep') process.exit(0);
if (tool === 'seq') {
  process.stdout.write('1 2 3\\n');
  process.exit(0);
}
const state = JSON.parse(fs.readFileSync('state.json', 'utf8'));
if (tool === 'aws' && args.slice(0, 4).join(' ') === 'eks describe-cluster --name thermora-cloud') {
  state.describes++;
  if (state.mode !== 'unavailable' && state.describes >= 2) state.exists = true;
  fs.writeFileSync('state.json', JSON.stringify(state));
  if (state.mode === 'unavailable' || state.describes === 1) process.exit(1);
  process.stdout.write(JSON.stringify({ cluster: { status: 'CREATING' } }));
} else if (tool === 'docker' && args[0] === 'start' && args[1] === 'floci-eks-thermora-cloud') {
  if (!state.exists) {
    process.stderr.write('Error response from daemon: No such container: floci-eks-thermora-cloud\\n');
    process.exit(1);
  }
  state.running = true;
  fs.writeFileSync('state.json', JSON.stringify(state));
} else if (tool === 'docker' && args[0] === 'exec' && args[1] === 'floci-eks-thermora-cloud' &&
  args[2] === 'kubectl' && args.includes('--raw=/readyz')) {
  state.probes++;
  fs.writeFileSync('state.json', JSON.stringify(state));
  process.exit(state.running && state.probes >= 2 ? 0 : 1);
} else process.exit(8);
`;
      for (const tool of ["aws", "docker", "sleep", "seq"])
        writeFileSync(join(root, "tools", tool), mock, { mode: 0o700 });
      writeFileSync(
        join(root, "state.json"),
        JSON.stringify({
          mode: state,
          exists: state === "stopped",
          running: false,
          describes: 0,
          probes: 0,
        }),
      );
      const result = spawnSync("/bin/sh", ["scripts/cloud.sh", "start"], {
        cwd: root,
        encoding: "utf8",
        timeout: 10000,
        env: { PATH: `${join(root, "tools")}:/usr/bin:/bin` },
      });
      expect(result.error).toBeUndefined();
      const calls = readFileSync(join(root, "calls.log"), "utf8");
      expect(calls).not.toMatch(
        /create-cluster|docker (run|pull)|kubectl apply/,
      );
      if (state === "unavailable") {
        expect(result.status).toBe(1);
        expect(result.stderr).toContain("did not become ready");
        expect(calls).not.toContain("docker start");
      } else {
        expect(result.status, result.stderr).toBe(0);
        expect(calls).toContain(
          "aws eks describe-cluster --name thermora-cloud",
        );
        expect(calls.match(/--raw=\/readyz/g)).toHaveLength(2);
      }
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  },
);
