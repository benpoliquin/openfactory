# Operations and configuration

For operators maintaining an enrolled installation. For a first setup, use [Getting started](getting-started.md); for work approvals, use [Workflows](workflows.md). Private phone deployment, pairing and recovery belong in [Phone intake](phone-intake.md), not this guide.

## Review the namespace before starting

OpenFactory's Go module/CLI is `openfactory` ([command source](../cmd/openfactory)). Defaults are:

| Resource | Default |
| --- | --- |
| Policy / public template | `config/openfactory.yaml` / [examples/openfactory.yaml](../examples/openfactory.yaml) |
| SQLite database | `openfactory.db` |
| Compose project; controller service/DNS | `openfactory`; `openfactory` |
| Local images | `openfactory-controller:local`, `openfactory-bb:local` |
| State volume key; private check socket directory | `openfactory-state`; `/run/openfactory-checks` |
| Global / per-project active attempts | `max_concurrent: 2` / `max_concurrent: 1` |
| Daily reserved dispatches | `max_calls_per_day: 30` |
| Attempts per Feature | `max_attempts: 3` |
| Attempt duration | `attempt_timeout_seconds: 1800` (30 minutes) |
| Prompt bound | `max_prompt_bytes: 24000`; Plan goal files are bounded to half this value (12,000 bytes by default) |

Project-owned environment names use `OPENFACTORY_*`. Upstream `BB_*`, `OMP_*`, provider and `COMPOSE_*` names are unchanged. An existing `.env`, especially `COMPOSE_PROJECT_NAME` and `COMPOSE_FILE`, overrides source defaults. Review the actual environment, project name and policy **before launch**; do not implicitly adopt another installation's state, BB home or volumes.

These source defaults are not a migration. The source rename did not deploy, migrate authentication/data, change provider access, push code, create a repository or publish anything. Existing private configuration, credentials, workspaces, containers and volumes remain the operator's responsibility and remain intact.

From the repository root, `./start.sh` creates `.env` and `config/openfactory.yaml` only if missing, builds images and starts the three-service stack. It never replaces an existing operator file. An empty configuration enrolls no repositories, approves no work and makes no idle model calls. **An existing installation can resume already approved work after restart.**

## Configure credentials with dedicated files

Use a least-privilege identity. Neither a host home, host SSH agent nor Docker socket is forwarded. Keep VPS secret files outside this checkout. Linux bind mounts retain ownership: dedicated secret files must be readable by container UID **10001**, normally owned by that UID with mode `0600`; do not make keys world-readable.

### HTTPS GitHub token

Create a private token file and set an absolute path in `.env`:

```dotenv
GITHUB_TOKEN_FILE=/absolute/path/to/dedicated-github-token
COMPOSE_FILE=compose.yaml:deploy/compose.token.yaml
```

The path above is a substitution, not an included secret. In the enrolled project use an HTTPS repository URL and:

```yaml
auth:
  mode: token
  token_file: /run/secrets/github_token
```

Grant only the repository contents/PR permissions actually required. Network authentication belongs to the controller, not a credential-bearing checkout URL.

### SSH Git, with a token for PRs

Create a dedicated key and independently verify GitHub's host keys. Set:

```dotenv
GIT_SSH_KEY_FILE=/absolute/path/to/dedicated-private-key
GIT_KNOWN_HOSTS_FILE=/absolute/path/to/verified-known-hosts
COMPOSE_FILE=compose.yaml:deploy/compose.ssh.yaml
```

Use an SSH repository URL and:

```yaml
auth:
  mode: ssh
  ssh_key_file: /run/secrets/git_ssh_key
  known_hosts_file: /run/secrets/git_known_hosts
```

Host checking is strict; unknown hosts are not accepted automatically. **PR creation still needs an API token.** Combine the token and SSH overlays, set all three host-file variables, and add `token_file: /run/secrets/github_token` alongside the SSH fields:

```dotenv
COMPOSE_FILE=compose.yaml:deploy/compose.token.yaml:deploy/compose.ssh.yaml
```

`auth.mode: none` is the default when `auth` is omitted. It adds no credentials: public HTTPS reads can work, but authenticated pushes and PRs cannot. An explicitly configured local/file repository can still be written by an authorized integrated target; `none` is not a read-only permission.

### Optional OpenRouter

Append [deploy/compose.openrouter.yaml](../deploy/compose.openrouter.yaml) to the existing `COMPOSE_FILE` list and set `OPENROUTER_API_KEY_FILE` to a dedicated private key file. The key mounts only into BB. Select an exact approved model available in the catalog, use capped credits and approved provider routing. There is no automatic provider or billing fallback. Credentials remain in the dedicated BB/provider state, not copied from the host.

After changing overlays, restart through `./start.sh` using the reviewed namespace. For policy/image changes, use the coordinated upgrade procedure below. Model login/catalog/`doctor` results do not prove subscription billing, Git write permission, token accounting or production readiness. See [Getting started](getting-started.md) for login and selector setup, and [Verification](verification.md#native-bb-route-provider-rejection) for the observed native-route provider prerequisite.

## Inspect, pause and settle work

Run these from the repository root. Uppercase IDs and hashes throughout these guides are substitutions for returned values, not runnable fixture identities.

```sh
./start.sh cli status
./start.sh cli questions
./start.sh cli events FEATURE_ID
./start.sh cli evidence FEATURE_ID
./start.sh cli pause
./start.sh cli resume
./start.sh cli cancel FEATURE_ID
./start.sh cli retry FEATURE_ID
./start.sh compose logs --follow openfactory
```

`pause` stops new admission; it is **not a remote kill switch**. Retry is allowed only for safely settled attempts. SIGTERM stops admission and cancels/drains owned work, but exiting the controller is not proof that a remote BB worker stopped. Compose supervises processes; only the controller owns feature retry policy. A singleton process lock prevents two controllers owning one ledger.

Unknown spawn/push/deploy outcomes retain capacity and delivery ownership. Inspect BB, Git and deployment systems, stop or settle external execution and record evidence before retrying. An operator can attest reconciliation as a **failure**, never as acceptance:

```sh
./start.sh cli resolve-failed FEATURE_ID --attempt EXACT_ATTEMPT_ID \
  --receipt 'Stopped remote execution; verified rollback; incident/evidence reference'
```

This records an attestation; it does not stop external work. Never use it while that work might still run. Retry or reapproval remains a separate decision. Do not delete volumes, ownership records or receipts to clear uncertainty. Phone command receipt reconciliation, device revocation and restore-auth procedures are in [Phone intake](phone-intake.md).

## Back up, stop and restore

```sh
./start.sh cli backup /state/backup.db
./start.sh stop
```

The backup command creates a consistent SQLite snapshot **inside the state mount**; it is not a complete installation backup. Securely back up SQLite, the complete artifact CAS and operator configuration together, plus BB/provider authentication, retained artifacts, experiment journals and any release-system state by their own procedures. Encrypt and restrict backup access. Stopping preserves named volumes.

Restore requires review of external execution and Git/release outcomes before admitting work. Schema migration is forward-only: **an older binary must not open schema 4**. A database alone cannot reconstruct missing CAS/configuration or undo external effects. Before exposing restored intake state or admitting restored work, follow the mandatory auth-generation rotation and fresh enrollment in [Phone recovery](phone-intake.md). Arbitrary SQLite/whole-volume rollback is not automatically detected by an external epoch file. Normal process restart preserves auth generation.

Workspace/object retention is operator-managed. Monitor disk use; failed attempts and evidence are not silently pruned.

## Upgrade policy, images or lane limits

Configuration changes invalidate old policy authority. Do not casually edit a live production policy or a watched bind mount.

1. Pause admission and let active lanes settle. Inspect dashboard/CLI evidence, BB, Git and release systems. Externally reconcile unknown outcomes; do not force-clear them to proceed.
2. Once drained, stop the stack and make the complete backup described above. Do not change policy while an old controller is watching it.
3. Install the reviewed controller/image and edit stopped configuration. Change only the intended settings. Per-project `max_concurrent` is **1..32**, default **1**, and stays within the reviewed global cap. Increasing a project cap adds neither global capacity nor daily calls nor release authority. Do not replace existing operator limits with example defaults or inflate the thirty-call allowance merely to add lanes.
4. Recreate **both `checks` and `openfactory`** after policy/image changes, then restart/recover. The controller mounts the configuration directory, but the checker's read-only single-file policy mount can retain an old inode after an atomic editor save. A stale execution-policy handshake fails closed. Recovery reacquires durable delivery ownership before admission; conflicting unresolved legacy owners block daemon startup while read/resolve CLI commands remain available.
5. Review the new exact policy hash; re-import/reapprove affected contracts and explicitly resume admission. Normalization/schema changes can change the hash. Omitted project concurrency and explicit `1` normalize identically in this version; a pre-upgrade approval is not new authority.

Global lanes default to **2**, project lanes to **1**, and daily reservations to **30**. Illustrations with three or more active lanes require explicit reviewed **global and project** opt-in; historical private lane settings are not public defaults. Features, Programs and Intake share these limits. Builds/checks/reviews may overlap, but delivery remains serialized per canonical repository and target branch. See [Architecture](architecture.md#scheduler-and-delivery-invariants).

## Network and dashboard settings

The [Compose stack](../compose.yaml) publishes BB `38886`, dashboard `38887` and optional intake `38889` on **host loopback only**. Internal container non-loopback binds are not permission for LAN/public exposure. Keep BB private; use an SSH tunnel for read-only remote access or the reviewed private HTTPS [phone frontdoor](phone-intake.md).

Checks have no external route by default and cannot reach BB's API. Bake dependencies into the check image, or explicitly append [deploy/compose.check-network.yaml](../deploy/compose.check-network.yaml) for trusted dependency downloads. This adds outbound/host-service access on a separate egress bridge; it is **not a hostile-code sandbox**. Research broker port `38888` is not host-published.

For a local binary:

```sh
openfactory --config config/openfactory.yaml run --dashboard-listen 127.0.0.1:38887
# Disable only the read-only dashboard, not an enabled intake frontdoor:
openfactory --config config/openfactory.yaml run --dashboard-listen ''
```

Addresses require a literal IP and numeric port. Non-loopback requires explicit `--dashboard-allow-non-loopback`, which adds no authentication. Compose binds `0.0.0.0:38887` inside the container but only `127.0.0.1:38887` on the host. Host authority is validated, CORS is not enabled and mutation methods return 405. Dashboard routes are `/`, `/features/ID`, `/api/status`, `/api/features/ID`, `/api/features/ID/events`, `/api/features/ID/evidence` and `/healthz`; JSON pagination is bounded and the overview accepts `project`/`state` filters. CLI JSON shapes are unchanged.

Dashboard evidence is generation-specific. It reports stages, waits, limits, sessions, delivery queues and evidence—not ETA, source, prompts, credentials, private workspace paths or integration tokens. The listener is supervised with the engine and drained before SQLite closes.

### BB plugin and snapshot bounds

The native [plugin](../plugins/openfactory) is npm package `bb-plugin-openfactory`, plugin ID `openfactory`. The image installs it with BB's supported installer; no BB fork or separate browser app is needed. After an explicit launch, open `http://127.0.0.1:38886/plugins/openfactory/board`. Restarts reload the packaged plugin while preserving that namespace's BB database/session history, not another installation's state.

The read-only board refreshes every four seconds while visible. It shows actual stages, dependencies, waits, sessions, candidate generations and PR links; an open PR is waiting to merge, and reviewed-only is not integrated. Recorded history shows known work/wait intervals through snapshot time, not forecasts. Older records expose only recoverable history. Internal discovery/intake steps are hidden from the product board but count toward capacity. The separate opt-in Intake panel is the authenticated command surface.

`OPENFACTORY_VIEW_URL` defaults to `http://openfactory:38887/api/status`. Set it in the **BB server environment** outside Compose. The trusted server-side bridge uses the endpoint's port with the dashboard's loopback Host authority; it grants no browser mutation authority. `OPENFACTORY_VIEW_MAX_PAGES` defaults to **20**, valid **1..100**, at up to 500 features/page. Whole retrieval is bounded to **8 seconds / 16 MiB**; overflow/error is explicit, never a silently partial board. The startup plugin health gate checks installation, not controller availability, avoiding a startup cycle.

### Intake policy reference

Enrollment, exact-origin setup, `BB_APP_URL` precedence and the optional `bb_host_authority` override are explained in [Phone intake](phone-intake.md). Effective nonsecret policy is available with `./start.sh cli config intake`. Intake defaults to disabled, opening no listener; enabling its existing frontdoor adds no service or scheduler.

| Group | Packaged settings |
| --- | --- |
| Origin | `enabled: false`; `external_origin` / `rp_id` empty; `rp_name: OpenFactory Intake`; fixed `public_path: /openfactory-intake` |
| Listener | `bind: 127.0.0.1:38889`; `allow_non_loopback: false`; runtime `--command-listen` / `--command-allow-non-loopback` |
| Text/output bytes | `max_turn_bytes: 16384`; `max_turns: 256`; `max_text_bytes: 2097152`; `max_canonical_bytes: 262144`; `max_output_bytes: 262144` |
| Draft/history | `max_draft_calls: 12`; `history_default: 50`; `history_max: 200` |
| Auth lifetimes, seconds | `pairing_ttl_seconds: 600`; `challenge_ttl_seconds: 120`; `session_idle_seconds: 43200`; `session_absolute_seconds: 604800`; `credential_ttl_seconds: 7776000` |
| Auth bounds | `max_credentials: 10`; `auth_rate_limit: 30`; no inline auth secrets |

Protocol text/output/draft maxima may be lowered, not raised. Limits must be YAML integer scalars, flags actual booleans; fractional, negative and over-cap limits fail closed. Origin/RP and optional top-level `bb_host_authority` are policy-bound; runtime listener flags do not alter the policy hash.

## Signed webhooks

Configure `webhook_url` and `webhook_secret_file`, append [deploy/compose.webhook.yaml](../deploy/compose.webhook.yaml), and set `WEBHOOK_SECRET_FILE`. Notifications use a durable per-destination outbox with capped retries/backoff and expiry. Redirects are refused; delivery is at-least-once.

Payloads contain event/feature identifiers, kind/status and creation time, not source, prompts, logs or credentials. Verify `X-OpenFactory-Signature` as HMAC-SHA256 of `X-OpenFactory-Timestamp + "." + rawBody`, reject old timestamps and deduplicate `X-OpenFactory-Event-ID`. Acceptance or a webhook response never authorizes a release.

## Development and compatibility

Requires **Go 1.26+**, **Git 2.38+** (`merge-tree --write-tree`) and **Docker Compose 2.24.4+** for deployment. The image enforces the Git minimum; `run` and `doctor` check capabilities. Images include Go, Node/npm, Bun, Python, Git and build tools; add project-specific toolchains to the Dockerfile rather than exposing the host Docker daemon.

```sh
go test ./...
go vet ./...
go build ./cmd/openfactory
```

Without `check_socket`, local checks execute with the local user's filesystem authority. Docker is the recommended isolation boundary, not a claim of adversarial containment.

For plugin development against **BB 0.47.1**:

```sh
cd plugins/openfactory
npm ci --include=dev
npm run typecheck
npm test
npm run build
BB_SERVER_URL=http://127.0.0.1:38886 bb plugin install . --yes
```

Pinned integrations are **BB 0.47.1**, **omp 18.10.3** and plugin SDK **0.6.79**. Upgrades require compatibility verification. Tests should exercise behavior and failure transitions rather than copied constants/source-text wiring; local Git/HTTP fixtures do not substitute for a live pinned integration or production qualification. Never run untrusted fork code with privileged CI secrets. Dependency updates and controller self-changes require separate operator review; the factory must not auto-upgrade its own executable/policy through its release lane.

See [Security](security.md) for trust limits, [Architecture](architecture.md) for implementation contracts and [Verification](verification.md) for the distinctly bounded historical evidence. [Back to README](../README.md).
