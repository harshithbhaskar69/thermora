# Working on Thermora

Thermora is an independent, local heat-pump control and flexibility reference project. It is not approved to operate real equipment.

## Read first

1. [README](README.md) for purpose, prerequisites, setup and runnable checks.
2. [Architecture](docs/architecture.md) for boundaries, control flow and limitations.
3. [API schemas](api/), [Taskfile](Taskfile.yml) and relevant source before changing behavior or interfaces.

## Boundaries

- Follow the user's authorized task scope. Documentation approval does not authorize application implementation, deployment, commits, or real-device access.
- Prefer the existing module, standard library, native platform, and smallest complete change. Keep the cloud API and workers in one Go application until an actual lifecycle or scaling boundary requires separation.
- Keep equipment simulation, edge translation, manufacturer-cloud integration, and cloud product/control logic distinct. Equipment Modbus stays on the private equipment network.
- Use one control resolver. UI actions, price optimization, and flexibility events submit intentions; they must not independently write equipment.
- Treat HTTP, MQTT, device data, and imported scenarios as trust boundaries. Enforce ownership, units, schema validation, expiry, and idempotency there.
- Preserve the distinction between broker acknowledgement, device acceptance, register application, and subsequent telemetry. Preserve wall time versus simulation time.
- Never claim real savings, measured capacity, production parity, safety certification, or successful tests without corresponding evidence.
- Do not copy third-party assets, confidential material, personal identifiers or unnecessary proprietary text into the product or docs.

## Validation and documentation

- For implementation work, run the smallest meaningful checks for the changed behavior, including negative and recovery cases when relevant. Use the commands in README.md and Taskfile.yml.
- Use the pinned mise environment and Taskfile: `mise exec -- task check` for lint/contracts/unit tests, `task security` for online dependency checks, and the relevant integration/browser tasks for behavior changes. `task ci` adds dependency consistency. Keep CI and local commands aligned; do not reintroduce Make or global tool installation requirements.
- Lint all maintained code, including integration tests. Fix findings; any suppression must name the rule and explain the verified exception. Never manually format generated contracts or supplied reference material. Update Task's package list when adding a new Go source root.
- HTTP contracts are specification-first; regenerate committed Go/TypeScript outputs and check compatibility. Never hand-edit generated files.
- Keep MQTT schemas, Modbus register encoding and runnable conformance checks synchronized.
- Keep README.md authoritative for setup and docs/architecture.md for design. Update the relevant document with a behavior change; record design tradeoffs in architecture.md rather than adding separate planning documents.
- Label commands, compatibility checks, and performance targets as proposed until implemented and verified. Record exact versions and observed results when a spike is completed.
- Keep credentials and local state out of Git. Do not install, contact external accounts, change global contexts, or run destructive reset commands unless the active task authorizes those actions.
