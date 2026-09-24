import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const script = fileURLToPath(
  new URL("../../scripts/check-tools.sh", import.meta.url),
);

test.each([
  "",
  "go version",
  "go tool compile -V",
  "node --version",
  "npm --version",
  "aws --version",
  "task --version",
  "golangci-lint version",
])("toolchain check rejects an overridden tool: %s", (mismatch) => {
  const root = mkdtempSync(join(tmpdir(), "thermora-toolchain-test-"));
  try {
    writeFileSync(
      join(root, "mise"),
      '#!/bin/sh\nshift 2\nexport THERMORA_TEST_PINNED=1\nexec "$@"\n',
      { mode: 0o700 },
    );
    for (const tool of ["go", "node", "npm", "aws", "task", "golangci-lint"]) {
      writeFileSync(
        join(root, tool),
        `#!/bin/sh
if [ "\${THERMORA_TEST_PINNED:-}" != 1 ] && [ "\${THERMORA_TEST_MISMATCH:-}" = "${tool} $*" ]; then
  printf '%s\\n' 'global version'
else
  printf '%s\\n' 'pinned version'
fi
`,
        { mode: 0o700 },
      );
    }
    const result = spawnSync("/bin/sh", [script], {
      encoding: "utf8",
      timeout: 5000,
      env: {
        PATH: root,
        THERMORA_TEST_MISMATCH: mismatch,
      },
    });
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(mismatch ? 1 : 0);
    if (mismatch) {
      expect(result.stderr).toContain(mismatch);
      expect(result.stderr).toContain('mise en -s "bash --noprofile --norc"');
    }
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
