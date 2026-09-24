# Thermora Cloud

Thermora is a local heat-pump optimization and energy-flexibility platform. Connect simulated equipment, set comfort preferences, run price-aware schedules, and follow commands through to changed equipment behavior and telemetry.

The React dashboard, Go backend, PostgreSQL and MQTT broker run locally with Docker and Kubernetes. Equipment is simulated; the project does not operate real heat pumps or participate in electricity markets. See the [architecture](docs/architecture.md) for the system diagram, control sequence and design tradeoffs.

Select a building and heating zone, then use **Status**, **Schedule**, **Statistics**, **Activity** and **Settings**. Status shows current readings and overrides; Activity distinguishes delivery from equipment confirmation. Charts support linked inspection, zoom, series selection and data tables. Appearance initially follows your system; the sun/moon button at the top right saves a Light/Dark preference. Fonts, charts and the API explorer are bundled locally.

## Prerequisites

- **macOS or Debian/Ubuntu Linux**, Git and a terminal. Automatic hostname/certificate installation supports these hosts; other environments can use the localhost option below.
- **Docker running**, with [Docker Compose v2](https://docs.docker.com/compose/install/). Docker Desktop includes Compose. Your user must be able to run Docker commands, and `/var/run/docker.sock` must be available. The cloud profile creates a privileged local K3s node through Floci.
- **[mise](https://mise.jdx.dev/installing-mise.html) 2026.9.12 or newer** on your PATH. It installs pinned Go, Node.js, AWS CLI, Task and quality tools from [mise.toml](mise.toml) and [mise.lock](mise.lock).
- **Internet access for first setup** to download tools, dependencies and images. A prepared cloud can restart offline with `task cloud:resume`.

Go race tests also require a C compiler: Xcode Command Line Tools on macOS or `build-essential` on Ubuntu. No AWS account, real AWS credentials, separate Kubernetes installation or mkcert installation is required.

## Start the cloud

After cloning, open a terminal in the repository root:

```sh
mise trust
mise install --locked
mise en -s "bash --noprofile --norc"
```

Use the full command above, including `-s "bash --noprofile --norc"`, to open a [project shell](https://mise.jdx.dev/cli/en.html). The Bash options skip personal startup files so Homebrew, nvm or other shell setup cannot replace the pinned tools. All commands below run inside it without a `mise exec` prefix. Use `exit` to leave; enter it again for later sessions.

After tool versions change, leave the old project shell, run `mise install --locked` and enter again. `task setup` and `task doctor` reject tools that differ from the mise environment, including a Go compiler selected through a stale `GOROOT`.

```sh
task setup
task doctor
task cloud:local-install
task cloud:up
task cloud:doctor
```

`cloud:local-install` requests administrator access to add loopback host entries and trust Thermora's public development CA. Thermora generates its certificates using Go; no public domain is registered. Keep host ports **443**, **4566** and **16510** available.

The installer can reclaim Thermora's exact generated loopback entry after `.local/` is recreated; custom/conflicting entries are still rejected. Before deliberately deleting local state, run `task cloud:local-uninstall` to remove the old CA trust. Local commands use `go run`, with `-exec sudo` for administrator actions: compilation runs as your user, with no saved `thermora-admin` executable. Go still uses its normal build cache; application binaries remain inside Docker images.

`cloud:up` builds the application, starts Floci, provisions the local Kubernetes cluster, applies migrations and starts one simulated heat pump. Later runs preserve accounts, devices and history.

Open **[cloud.thermora.io](https://cloud.thermora.io/connect)**. The API explorer is at **[/docs/](https://cloud.thermora.io/docs/)**; sign in to the dashboard first to try authenticated product endpoints.

## Connect a heat pump

1. Create a Thermora account, or use the `login` and `password` in `.local/cloud/seed.json`.
2. Choose **Connect manufacturer account**. At `manufacturer.thermora.io`, use the separate `manufacturer_login` and `manufacturer_password` in `.local/cloud/onboarding-access.json`.
3. Authorize access, select the pump, and enter the `partner_access_code` from that same access file. Add building details and accept the simulation consent.
4. Wait for fresh telemetry. Apply a temperature override within the comfort bounds and follow its acceptance, application and telemetry confirmation.
5. Cancel the override, select a price/weather scenario and enable optimization. Inspect temperature, energy, cost and the schedule; disable optimization whenever needed.

**The two accounts are separate.** Start manufacturer sign-in with **Connect manufacturer account** in Thermora; opening the manufacturer root URL redirects to Cloud's `/connect` page. `onboarding.json` is internal provisioning data. All `.local/` files are private and ignored by Git. `task cloud:access` shows active URLs and the credential file guide without printing passwords.

Simulation defaults to **60×** speed. Building **Settings → Simulation run** supports pause, pacing and a new run; reset preserves ownership/history and disables automation. Statistics distinguish recorded simulation from model comparisons. Savings are estimates and may be unavailable or negative.

## Everyday commands

| Command                                 | Purpose                                                                              |
| --------------------------------------- | ------------------------------------------------------------------------------------ |
| `task cloud:down`                       | Stop cloud, cluster and equipment; retain data.                                      |
| `task cloud:resume`                     | Restart prepared resources without builds or image pulls.                            |
| `task cloud:up`                         | Rebuild and apply source changes; retain data.                                       |
| `task cloud:doctor`                     | Check local names, certificate trust and readiness.                                  |
| `task cloud:kubectl -- get pods`        | Inspect the local `thermora` namespace without changing your Kubernetes context.     |
| `task cloud:device:add DEVICE=helsinki` | Add an independent pump; reusing its name preserves identity.                        |
| `task cloud:local-uninstall`            | Remove this installation's host entries and CA trust.                                |
| `task nuke`                             | Confirm permanent removal of both profiles, Docker resources and private local data. |
| `task --list`                           | List all commands.                                                                   |

The added pump's invitation is in `.local/cloud/devices/helsinki/onboarding-access.json`. Connect it to a new building or a building you own. Setup does not transfer an already enrolled pump. Exiting the project shell does not stop containers; use `task cloud:down` when finished. Do not delete individual state or credential files to reset a device.

To start completely fresh, run `task nuke` and answer `y` to its confirmation (Enter or `n` cancels). With Docker running, it runs `cloud:local-uninstall` first, then removes both profiles' containers, networks and volumes, the Floci Kubernetes node and its storage, Thermora-built images, `.local/` **including all backups and credentials**, and `.task/`. Cleanup stops on errors and preserves local credentials for retry. Images still used by other containers, shared base images, global Docker build caches, source and installed tools/dependencies remain. It refuses remote Docker contexts. Repeat the setup steps afterward; new accounts and credentials will be generated.

<details>
<summary>Inspect local AWS resources</summary>

With the cloud running, pass ordinary AWS CLI arguments after `--`:

```sh
task cloud:aws -- s3 ls
task cloud:aws -- s3 ls s3://thermora-reports/ --recursive
task cloud:aws -- eks list-clusters
task cloud:aws -- eks describe-cluster --name thermora-cloud
task cloud:aws -- ec2 describe-vpcs
task cloud:aws -- ec2 describe-subnets
task cloud:aws -- iam list-roles
```

The [Floci profile](deploy/cloud/aws.config) sets local dummy credentials, `us-east-1`, JSON output and path-style S3 addressing. The task uses this profile and fixes the endpoint to `http://127.0.0.1:4566`, overriding ambient AWS credentials and endpoints without changing `~/.aws/`. Standard options such as `--query` and `--output table` work. AWS CLI comes with `mise install --locked`; no `aws configure` step is needed. For Kubernetes workloads, use `task cloud:kubectl -- get pods`.

</details>

<details>
<summary>Localhost option: no host entries or system trust changes</summary>

After `task setup`, skip `cloud:local-install` and run:

```sh
task cloud:up LOCALHOST=1
task cloud:access
```

Open **https://localhost:9443/connect** and **https://manufacturer.localhost:9444**, and accept development certificates for both origins. `cloud:doctor` checks system trust, so browser exceptions alone will not make its TLS check pass. To switch to named domains, run `task cloud:local-install` followed by `task cloud:up`.

</details>

<details>
<summary>Optional fleet and backup demonstrations</summary>

```sh
task setup:browser
task cloud:fleet COUNT=10
```

This provisions a reusable fleet and runs a browser dispatch/cancellation check. Sign in with `.local/cloud/operator-access.json` for **Fleet operations**. The catalog cap is 25 devices, including other fixtures; begin with one pump on smaller machines.

```sh
task backup
task restore FILE=.local/backups/cloud-XXXXXXXX
```

Replace the example path with the directory printed by `backup`. Backup briefly pauses the system and then resumes it. `restore` verifies a snapshot in an isolated database; it does not replace live state. Backups contain credentials and must remain private.

</details>

<details>
<summary>Optional Compose-only development profile</summary>

`task demo` starts the direct edge-control loop without Kubernetes at **https://localhost:8443**. Accept its development certificate and use `.local/demo-access.json` for login and the one-use `claim_code`. `task demo:seed` refreshes an unused code; it preserves already claimed devices. Stop with `task down`.

Compose uses separate accounts/state under `.local/`; cloud uses `.local/cloud/`. Use the matching commands and access files. `task demo:onboarding` also runs manufacturer onboarding in Compose. The cloud profile above is the main walkthrough.

</details>

## Validate changes

```sh
task check              # monorepo lint, formatting, contracts and unit/race tests
task security           # dependency advisories; requires internet
task test:integration   # isolated PostgreSQL/MQTT containers; requires Docker
```

For the running cloud's browser and recovery checks:

```sh
task setup:browser
task test:ui          # deterministic UI checks; no running cloud required
task test:cloud
task test:recovery
```

On Linux, Playwright may need [browser system libraries](https://playwright.dev/docs/browsers#install-system-dependencies). `test:cloud` uses a dedicated test pump; `test:recovery` briefly interrupts the local broker/database. `task format:go` and `task lint:go` target Go; `task format` formats the maintained repository. [GitHub Actions](.github/workflows/ci.yml) runs these checks plus browser, fleet and backup verification jobs.

For setup problems, run `task doctor` and `task cloud:access`. Use `task cloud:doctor` for domain/trust failures and `task cloud:kubectl -- get pods` for cluster readiness. If port 443 is occupied, use the localhost option. Read [Architecture](docs/architecture.md) for design details and limitations.
