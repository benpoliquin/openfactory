# OpenFactory

A continuous, approval-gated engineering loop: **Go + SQLite → BB → omp → Astra / optional OpenRouter → checks → independent review → authorized delivery**.

General-purpose, multi-repository, MIT licensed. Runs with Docker Compose. No model calls while idle; no production authority by default.

## Source rename and fresh namespace

OpenFactory is the source/module name (`openfactory`) and CLI (`cmd/openfactory`, built as `openfactory`). The default policy is `config/openfactory.yaml`, the public template is `examples/openfactory.yaml`, and the default database is `openfactory.db`. Project-owned environment variables use `OPENFACTORY_*`; upstream `BB_*`, `OMP_*`, provider and `COMPOSE_*` variables keep their upstream names.

The Compose default project is `openfactory`, with controller service/DNS `openfactory`, images `openfactory-controller:local` and `openfactory-bb:local`, and the `openfactory-state` volume key. The private check socket directory is `/run/openfactory-checks`. These source defaults are **not a migration or an automatic adoption of existing state**. An existing `.env`, including `COMPOSE_PROJECT_NAME`, can override the new namespace. Before any launch, explicitly review a fresh environment, Compose project name and configuration; do not reuse existing operator state, BB homes or volumes implicitly.

Existing installations and private configuration, credentials, state, workspaces, containers and volumes remain intact. The source rename does not start or deploy services, migrate data or authentication, alter model/provider access, push code, create a repository or publish the project. The commands below are explicit operator procedures, not actions performed by the rename.

## Start

Requires Docker with Compose **2.24.4+**, outbound internet, and your own model access. Local controller runs require **Git 2.38+** (`merge-tree --write-tree`); the image enforces that minimum and `run`/`doctor` check capabilities.

```sh
./start.sh
```

This creates `.env` and `config/openfactory.yaml` **only if missing**, builds the images, and starts three services in the reviewed namespace. It does not replace an existing `.env` or convert private configuration. Open **http://127.0.0.1:38887** for the read-only OpenFactory dashboard and **http://127.0.0.1:38886** for BB agent sessions. Both are published on host loopback only. The empty default configuration does not launch engineering jobs or spend model quota.

Authenticate inside the dedicated BB container, then select actual available model IDs:

```sh
./start.sh bb omp login
./start.sh bb omp models ls --json
```

Set `OPENFACTORY_ASTRA_MODEL` in `.env` to your exact Codex Astra selector. Optionally set `OPENFACTORY_FAST_MODEL` to an approved OpenRouter model. Do not assume a model name implies subscription billing. Provider credentials are kept in the BB volume, not copied from your host.

Edit `config/openfactory.yaml`: enroll a repository, its branch, trusted status, authentication, and real `unit`/`smoke` argv checks. The commented example is a template, not a runnable fake application. Then:

```sh
./start.sh
./start.sh cli doctor
```

`doctor` verifies Git capabilities, BB/provider/model availability and configured file presence; it does **not** prove model authentication, Git write access, billed-token accounting, or production readiness.

Checks have **no external network route by default** and cannot reach BB's API. Bake dependencies into the check image, or explicitly append `deploy/compose.check-network.yaml` to `COMPOSE_FILE` for trusted dependency downloads. That opt-in restores outbound/host-service access; it is not a hostile-code sandbox.

## Feed the loop

Copy `examples/feature.md` into `config/`, replace its project and acceptance cases with a real feature, then:

```sh
./start.sh cli enqueue /config/feature.md
./start.sh cli approve csv-quoted-fields --revision HASH_FROM_ENQUEUE
./start.sh cli status
./start.sh cli evidence csv-quoted-fields
```

Approval binds the exact feature revision and operator policy. `depends_on` orders work. Targets are `reviewed`, `pull_request`, `integrated`, `staging`, or `production`; the last two require enrolled release profiles. A PR is not an integrated dependency.

For a larger goal, copy/edit `examples/goal.md` into `config/`:

```sh
./start.sh cli plan myapp /config/goal.md
./start.sh cli questions
```

Planning is read-only. It imports proposed feature contracts; it does not approve their implementation. Approve the ones you want. The daemon then continuously picks ready work within concurrency, daily-call, timeout, and retry limits.

## Discovery Programs

A Program turns a research objective into **unapproved** implementation contracts, not delivered code. It shares the existing scheduler, project/global lanes and daily-call budget; lifetime Program bounds survive revisions. Discovery is implemented but **not rolled out to the live operator**. Historically recorded global and project lane caps are operator-specific settings, not new defaults or evidence that the intentionally stopped stack is running. The historical three-lane smoke below is separate evidence.

Review `examples/program.json`, `examples/program.bundle.json`, `examples/experiment.py`, and `examples/experiment.profile.yaml`. Enroll the example's `discovery-fixture` project with real checks and merge only reviewed execution profiles into stopped operator configuration. Copy the three Program inputs into `/config` together (the bundle manifest resolves declared files relative to itself). The example explicitly consents to sending its declared script to the configured model provider:

```sh
./start.sh cli program import /config/program.json --bundle /config/program.bundle.json
./start.sh cli program approve event-ordering-discovery --revision REVISION_HASH_FROM_IMPORT
./start.sh cli program status event-ordering-discovery
./start.sh cli program questions event-ordering-discovery
./start.sh cli program answer event-ordering-discovery --revision EXACT_CURRENT_REVISION_HASH --file /config/answer.json
# The answer returns a new revision: inspect and approve that exact revision.
./start.sh cli program approve event-ordering-discovery --revision NEW_REVISION_HASH
./start.sh cli program architecture event-ordering-discovery
./start.sh cli program evidence event-ordering-discovery --artifact ARTIFACT_ID
./start.sh cli program approve-architecture event-ordering-discovery --revision EXACT_CURRENT_PROGRAM_REV_HASH --artifact ARCHITECTURE_CONTENT_SHA
./start.sh cli program experiments event-ordering-discovery
./start.sh cli program approve-experiment event-ordering-discovery --revision EXACT_CURRENT_PROGRAM_REV_HASH --contract EXPERIMENT_CONTRACT_SHA
./start.sh cli program evidence event-ordering-discovery
./start.sh cli program features event-ordering-discovery
./start.sh cli approve PROPOSED_FEATURE_ID --revision EXACT_FEATURE_REVISION_HASH
```

Substitute returned identifiers/hashes, not example literals. Answers are strict JSON `ProgramAnswer` objects (`question_id`, `text`); the command binds operator identity and the exact question revision. Old-version answers fail rather than silently answering a newer question. Architecture and each experiment require separate exact-content grants on every materially revised iteration. Inspect evidence before granting authority; approval of a Program never approves proposed product work.

For public-only research, remove the bundle seed from the explicit Program JSON, set `disclosure.private_material_to_models` to `false`, and import **without `--bundle`**. Uploading a private bundle or importing an unapproved proposal is shape validation and storage, **not model consent**: these operations can succeed with `private_material_to_models: false` and make zero model calls. Execution/research approval remains strict and rejects missing required context/provider disclosure; it never silently flips consent. Bundles contain only declared regular files, not a directory crawl. Never feed a browser's private `file://` path directly into research. Public URLs/search topics are explicit disclosures too. Questions block without repeated model calls; missing hardware or other prerequisites remain waits, not fabricated observations. See [DESIGN.md](DESIGN.md#discovery-programs) for evidence, execution and authority boundaries.

Read-only Program views are `/programs`, `/programs/ID`, `/api/programs` and `/api/programs/ID` with detail sections for questions, architecture, experiments, evidence, features and events. The BB OpenFactory sidebar includes Programs. Lists paginate; history and current-stage output labels are provenance, not approval authority.

## OpenFactory Intake: private phone-to-contract workflow

**Opt-in, disabled by default, and not rolled out to the live operator.** OpenFactory Intake is a native BB sidebar panel at `/plugins/openfactory/intake/`, not BB's native worker-chat composer. Use the phone OS keyboard's dictation to enter **text**; there is no audio-upload/transcription service. The existing read-only OpenFactory board and Programs views remain read-only. Intake sends authenticated commands directly to the controller on the same private browser origin, never through mutation RPC, a bridge service key, or a model-created approval.

### Prepare private access

Use the complete [private HTTPS deployment recipe](examples/intake-private-https.txt) with [example policy](examples/openfactory.yaml). This is an operator procedure, not something `start.sh` deploys automatically. The operator stack is intentionally stopped; these instructions do not authorize starting it, changing provider access, or exposing it.

1. Drain/pause work, stop the stack, and back up SQLite, the complete artifact CAS, and operator configuration together; preserve BB/provider state by its existing procedure. The schema migration is forward-only: an older binary must not open schema 4. Review policy changes and recreate **both controller and checker** together; an atomic host edit can leave the checker's single-file bind mount on an old inode.
2. Choose the actual stable private HTTPS hostname. In reviewed configuration set:

   ```yaml
   intake:
     enabled: true
     external_origin: https://openfactory.example-tailnet.ts.net
     rp_id: openfactory.example-tailnet.ts.net
     public_path: /openfactory-intake
   ```

   Replace the example hostname everywhere. Origin must exactly match browser serialization: lowercase HTTPS scheme/hostname, no trailing slash, path, query, fragment or explicit default port. RP ID is the **exact hostname**, without scheme/port, not a parent-domain shortcut. Inspect effective nonsecret policy with `./start.sh cli config intake`.
3. Set `OPENFACTORY_BB_APP_URL=https://openfactory.example-tailnet.ts.net` in reviewed dotenv so Compose passes `BB_APP_URL` to BB. **Persisted BB configuration takes precedence** over environment; inspect/update it explicitly if necessary using the recipe's pinned BB CLI. `BB_SERVER_URL` is not the browser-origin allowlist. For the local Docker BB only, explicitly set `OPENFACTORY_BB_HOST_AUTHORITY=localhost:38886` if required by its accepted HTTP authorities. This optional override changes only HTTP Host, never connection DNS, URL, TLS SNI or certificate identity. Its empty default preserves remote HTTPS virtual hosts; never apply localhost automatically to remote BB.
4. Forward **all HTTPS paths intact to one frontdoor**, not separate BB/API mounts:

   ```sh
   tailscale serve status
   tailscale serve --bg --https=443 http://127.0.0.1:38889
   tailscale serve status
   ```

   Review existing Serve mounts and tailnet ACLs first; a more-specific bypass must not route around the frontdoor. Do not reset unrelated apps or use Funnel. Prefix stripping happens **inside** the frontdoor: `/openfactory-intake/*` reaches internal `/v1`, bare `/v1/*` is not public, and other paths proxy to configured BB with native WebSocket/SSE support. Do not strip the prefix at Tailscale. See [Tailscale Serve reference](https://tailscale.com/docs/reference/tailscale-cli/serve).

The enabled controller uses the existing optional listener **38889**, with no additional app, port or daemon. Compose publishes 38886, 38887 and optional 38889 on **host loopback only**; its explicit internal non-loopback command bind is not permission for LAN/public exposure. Native defaults are `127.0.0.1:38889`; `run --command-listen IP:PORT` and `--command-allow-non-loopback` control this listener, but no socket opens when intake is disabled.

Private browsers must use the frontdoor, **not direct BB 38886**. Worker-controlled attachment-content responses receive enforcing CSP `sandbox allow-scripts` **without `allow-same-origin`**, plus `nosniff`, including errors/304; existing CSP remains enforced. This gives attachment scripts an opaque origin rather than the approval origin. Operator cookies/CSRF headers are stripped before BB; native BB cookies and actual browser Origin remain. Forwarded identity headers confer no authority. No operator-auth mount, controller key, token, database or provider credential is added to BB. An equivalent private proxy must preserve paths, Origin, cookie Path and streams, without wildcard CORS or attachment-route bypasses.

Local isolated development may use exact `http://localhost:PORT` with RP ID `localhost` and the same-origin frontdoor. This is not a phone deployment recipe: raw-IP remote HTTP, TLS-warning bypasses, split frontend/API origins and disabling user verification are unsupported.

### Pair, confirm, then log in

Run from a trusted local terminal, never a BB thread or model prompt:

```sh
./start.sh cli operator create owner --label 'OpenFactory owner' --project myapp
./start.sh cli operator status owner
./start.sh cli operator pair owner --output /tmp/owner-pairing.json
```

Repeat `--project` for each explicitly authorized enrolled project. Pairing uses a random 256-bit secret in the URL **fragment**, never query parameters, with a default ten-minute lifetime. Output files are exclusive mode `0600`; with Compose the path is inside the container. Read it only in a trusted terminal as described in the recipe, then delete it. Omitting `--output` prints secret-bearing JSON to that terminal; do not put it in logs, tickets or prompts.

Open the link in the phone browser and register a passkey. Obtain the **full lowercase SHA-256 credential fingerprint only from that phone's successful registration screen**, then:

```sh
./start.sh cli operator pairing PAIRING_ID
./start.sh cli operator confirm PAIRING_ID --fingerprint FULL_LOWERCASE_SHA256
./start.sh cli operator devices owner
```

Pairing inspection deliberately never echoes the pending fingerprint or a confirmation code. A mismatch revokes the pairing: create a **new** pairing and repeat registration. Confirmation activates exactly that credential and consumes the pairing atomically. Registration is not login; perform a fresh passkey login afterward. Keys remain in the OS/authenticator, not BB/controller storage. The browser is still a trust boundary: passkeys prove user presence/verification, not that an authenticator displayed or understood contract text.

### Save, generate, review and hand off

1. Select an enrolled project and dictate/type the request. **Save** persists the request/turn only, with **zero model work** and no model-disclosure requirement. Project identity is immutable for that request; switching projects requires a new linked request.
2. **Generate Proposal** explicitly authorizes one bounded read-only draft of the **current saved context**, exact configured provider/model identifiers and selected bundle hashes shown by the server disclosure. **Send** composes Save then Generate with separate stable command receipts; saving alone never authorizes drafting. Uploading/importing material is not consent. A private upload may remain saved without being sent to a model.
3. Read the durable transcript and consequential questions; answer/refine and explicitly generate again as needed. Drafting has one restricted `intake_read` tool in an empty scoped workspace, no product-repository clone, arbitrary tools or second scheduler. It shares ordinary attempts/reservations, global/project lanes and the daily budget. The default request lifetime cap is **12 dispatches**, bounded by policy and not reset by edit/retry/restart. Existing lane enrollment is operator policy, not a new default. Progress/history polling makes no model calls; capacity waits retain saved work.
4. Inspect the exact canonical Feature Markdown, read-only plan, or Program JSON, source-byte SHA, domain revision, findings, assumptions and risks. Import/edit/export uses existing canonical formats; source changes invalidate old review. Shape-valid unapproved drafts, including private Program bundles without consent, may be saved/exported with no model use. **Approve and queue** is an execution grant, not an import-only fallback: absent required disclosure it is rejected before authority or partial import, leaving the draft editable.
5. Approve the exact reviewed card with a **fresh user-presence + user-verification passkey ceremony**. Grants bind current request/content/revision, command, policy, bundle and evidence, not model summaries. Feature queueing, Program revision, architecture, **each frozen experiment contract**, retry/resume and product approvals are distinct authority; none grants broader accounts, hardware, spend or production permissions.
6. Follow durable status and recorded BB worker links. `submitted` means handoff committed, **not delivered**. Plans publish unapproved child Features; planning answers create a new unapproved revision requiring review. Program answers similarly create a new revision; architecture/experiments and final proposed products still need their separate grants. Ordinary feature checks, independent review and delivery gates remain authoritative.

Missing resources or consequential decisions block for answers rather than inventing results. One active draft per request and superseded/uncertain attempt ownership remain settled through existing cancellation/reconciliation, not a blind replay. Cancellation is not proof a remote operation has stopped.

Queued drafts report the ordinary scheduler's concrete capacity/admission wait. Creating an internal step is not a running model call: only a successful Claim publishes `drafting` and binds its attempt. Active daily-call waits—and their clearing—follow the owned attempt; polling does not bypass admission or charge another dispatch.

For a failed Program worker, **Review resume** selects at most one current, settled failed step. Inspect its immutable input, internal Feature revision, latest attempt and unchanged attempt cap before granting a fresh passkey approval. Changed or uncertain ownership, a cancelled Program, or exhausted lifetime/attempt allowances reject recovery. An experiment retry also retains its exact frozen contract, bundle and execution-policy binding; conclusive supported or falsified results are not failures to rerun. `max_experiments` counts logical experiment steps, while physical retries remain bounded by ordinary attempt caps, fresh retry grants and per-run limits.

```sh
./start.sh cli intake status
./start.sh cli intake status --limit 50 --after LAST_RETURNED_REQUEST_ID
./start.sh cli intake status REQUEST_ID
./start.sh cli intake export REQUEST_ID --revision 1 --output /tmp/draft.md
```

Follow the returned `next_cursor` while `has_more`; history is bounded, not silently complete. Export writes exact immutable source bytes without rewriting/newline insertion, with source SHA separate from domain revision; use `.json` for Program drafts. Without `--output`, stdout is raw source, not JSON. Files are exclusive mode `0600`.

### Reconnect, revoke and restore safely

The browser journals pending command IDs, send/version fences and exact non-sensitive `intake.draft` disclosure metadata, scoped to origin, installation, auth generation and actor. Private dictation, canonical source, answer bodies, cookies and authentication material are never localStorage recovery data. For a lost response, reconcile its receipt and retry the **identical command with the same stable UUID**, never create a new ID to guess whether it ran. A generation/installation mismatch is unresolved and must not be automatically replayed. Normal process restart preserves auth generation.

```sh
./start.sh cli operator revoke-device CREDENTIAL_ID
./start.sh cli operator revoke owner
./start.sh cli operator auth-generation
./start.sh cli operator restore-auth --confirm invalidate-all-browser-auth
```

Revocation blocks future sessions/challenges/commands; it does **not** silently cancel already committed bounded drafts or historically approved budgeted work. Stop that work through explicit cancellation and ownership settlement. Restore rotates generation and revokes **all old credentials, sessions, challenges and pairings**; old passkeys cannot freshly log in. Fresh trusted-CLI pairing, phone registration and confirmation are required.

**Run restore-auth before exposing any restored database or admitting restored work**, then reconcile external attempt/experiment ownership before retry/resume. Arbitrary SQLite/whole-volume rollback is **not automatically detected**; there is no external epoch-file guarantee. Never recover by exposing unauthenticated endpoints, deleting ownership records or restoring only the database while losing its CAS/configuration. Keep pairing bodies, fragments, assertions, cookies, CSRF and private transcripts out of proxy logs.


## GitHub credentials

Use a dedicated, least-privilege identity. No host home, SSH agent, or Docker socket is forwarded.

**HTTPS token:** create a private token file, set `GITHUB_TOKEN_FILE=/absolute/path` in `.env`, and set:

```dotenv
COMPOSE_FILE=compose.yaml:deploy/compose.token.yaml
```

In the enrolled project use `auth: {mode: token, token_file: /run/secrets/github_token}` with an HTTPS repository URL. Grant only required repository contents/PR permissions.

**SSH:** create a dedicated key and independently verify GitHub's host keys. Set `GIT_SSH_KEY_FILE` and `GIT_KNOWN_HOSTS_FILE`, and use:

```dotenv
COMPOSE_FILE=compose.yaml:deploy/compose.ssh.yaml
```

Use `auth: {mode: ssh, ssh_key_file: /run/secrets/git_ssh_key, known_hosts_file: /run/secrets/git_known_hosts}` and the SSH repository URL. Host checking is strict. **PR creation also requires an API token**: combine both overlays and set `token_file` alongside SSH fields.

Linux bind mounts retain ownership: secret files must be readable by container UID **10001**. Use dedicated files owned by that UID with mode `0600`; do not make keys world-readable. On a VPS, keep secret files outside this checkout. Restart with `./start.sh` after changing overlays.

For OpenRouter, append `deploy/compose.openrouter.yaml` and set `OPENROUTER_API_KEY_FILE`. It mounts only into BB. Use capped credits and approved provider routing; there is no automatic provider/billing fallback.

## Parallel feature lanes and delivery

The global `max_concurrent` remains the total active-attempt limit (default **2**); `max_calls_per_day` remains **30**. Each enrolled project has its own `max_concurrent`, default **1**, valid **1..32**. Opting a project into `max_concurrent: 2` permits two independent features to build, check, and receive independent review in separate attempt workspaces, still within the global and daily-call limits. Dependencies still require integrated receipts; higher concurrency does not approve work or grant release authority.

Delivery is a durable FIFO lane per canonical repository and target branch, shared even by differently named projects or GitHub HTTPS/SSH aliases. Local path/file URL aliases resolve symlinks. Staging and production also hold a repository/environment resource. Ownership lasts through reconciliation, fresh checks/review, PR or push, and any deployment/observation/rollback.

When a target moves, the controller reconciles the immutable original feature delta onto the fetched head using Git's merge machinery in a private workspace. A changed candidate gets a new generation, configured checks and independent review before delivery. Conflicts and rewritten history block without agent-authored conflict resolution or remote writes. The final push names the exact expected old SHA and requires a direct-parent fast-forward candidate. Unknown external outcomes retain ownership rather than allowing another feature to guess or overwrite them. A reviewed-only target produces a local receipt without entering delivery.

## Read-only dashboard

The dashboard shows stages, admission waits, call/concurrency limits, session IDs, candidate generations, checks/review evidence, and delivery ownership/queue position. It does not enqueue, approve, cancel, retry, deploy, or call models. Evidence from earlier generations cannot stand in for current candidate proof. Source, prompts, credentials, workspace paths, and integration tokens are not dashboard overview fields.

For a local binary:

```sh
openfactory --config config/openfactory.yaml run --dashboard-listen 127.0.0.1:38887
# Disable the read-only dashboard (intake's opt-in frontdoor is separate):
openfactory --config config/openfactory.yaml run --dashboard-listen ''
```

Listener addresses require a literal IP and numeric port. Non-loopback binds require `--dashboard-allow-non-loopback`; this is not authentication. Compose explicitly binds `0.0.0.0:38887` inside the container and publishes **only** `127.0.0.1:38887` on the host. Keep that host publication private; use an SSH tunnel for remote access. Requests validate their Host authority, have no CORS permission, and mutation methods return 405.

Read-only routes: `/`, `/features/ID`, `/api/status`, `/api/features/ID`, `/api/features/ID/events`, `/api/features/ID/evidence`, `/healthz`. JSON lists have bounded pagination and the overview supports `project`/`state` filters. Existing CLI JSON shapes are unchanged. The listener is supervised alongside the engine and drained before SQLite closes.

BB's OpenFactory board and recorded timeline use the same read-only API through a trusted server-side bridge. `OPENFACTORY_VIEW_URL` defaults to `http://openfactory:38887/api/status`; the bridge sends the dashboard's loopback Host authority rather than exposing access in browser code. `OPENFACTORY_VIEW_MAX_PAGES` defaults to **20**, valid **1..100**, with up to 500 features per page. Snapshot retrieval is bounded to 8 seconds and 16 MiB total; exceeding a bound produces an explicit error, never a silently partial board.

## BB OpenFactory plugin

The native BB plugin lives in `plugins/openfactory`, with npm package name `bb-plugin-openfactory` and BB plugin ID `openfactory`. After an explicit `./start.sh` launch in the reviewed fresh namespace, open **OpenFactory** in BB's sidebar at **http://127.0.0.1:38886/plugins/openfactory/board**. The image installs it automatically through BB's supported plugin installer; no BB source fork or separate browser application is required. Restarts within that namespace reload the packaged plugin while preserving its BB database and session history; this does not adopt another installation's BB state.

Each feature has a lane across **queued → building → checking → reviewing → waiting to merge → integrated**, with its actual controller stage, dependencies, waiting reason, capacity, candidate generation, checks, session navigation, and PR link. A created PR stays **waiting to merge**; a review-only result remains **reviewed**, not integrated. Search and project filters narrow the board.

Expand **Recorded history** for a shared-axis timeline of recorded stage boundaries, parallel work, and waiting intervals. Stripes distinguish waiting from work; open intervals run only through the snapshot time. No future completion dates are forecast. Older records show only recoverable history and known session identities.

The board refreshes every four seconds while visible and reports upstream errors or stale snapshots rather than inventing progress. BB reads `/api/status` server-side; controller state remains authoritative and the plugin's RPC bridge has no mutation methods. The separate opt-in OpenFactory Intake panel uses the authenticated same-origin controller API described above. The plugin startup health gate verifies installation, not controller availability, so service startup has no dependency cycle.

For development against an existing **BB 0.47.1** server:

```sh
cd plugins/openfactory
npm ci --include=dev
npm run typecheck
npm test
npm run build
BB_SERVER_URL=http://127.0.0.1:38886 bb plugin install . --yes
```

Set `OPENFACTORY_VIEW_URL` in the **BB server's environment** to the controller's `/api/status` endpoint when not using Compose. The bridge uses that endpoint's port for the dashboard's loopback Host authority. The SDK is pinned to **0.6.79**; BB upgrades require compatibility verification.


## Operate

```sh
./start.sh cli questions                   # approvals and blocked work
./start.sh cli events FEATURE_ID
./start.sh cli pause                       # stop new admission, not a remote kill switch
./start.sh cli resume
./start.sh cli cancel FEATURE_ID
./start.sh cli retry FEATURE_ID            # only safely settled attempts
./start.sh cli backup /state/backup.db
./start.sh compose logs --follow openfactory
./start.sh stop                            # preserves named volumes
```

Unknown spawn/push/deploy outcomes retain ownership and block blind retries. First inspect BB/Git/deployment state and settle external execution. An operator can then record that reconciliation as a **failure**, never acceptance:

```sh
./start.sh cli resolve-failed FEATURE_ID --attempt EXACT_ATTEMPT_ID \
  --receipt 'Stopped remote execution; verified rollback; incident/evidence reference'
```

This is an attestation, not a stop command. Never use it while external work might still run. Retry or reapproval is a separate decision. Configuration edits invalidate old policy authority; do not edit a watched production policy casually.

### Upgrading policy or enabling project parallelism

1. `./start.sh cli pause` to stop new admission. Let active lanes settle; inspect the dashboard, CLI evidence, BB, Git, and any release systems. An uncertain operation must be externally reconciled, not force-cleared to make an upgrade proceed.
2. Once drained, stop the controller/stack and back up SQLite plus BB/auth/artifacts and experiment journals. Do not change a bind-mounted policy while an old controller is still watching it.
3. Install the new controller/image and edit the stopped operator policy. For example, change only the intended project's `max_concurrent` within the already reviewed global capacity, without changing the existing global cap, `max_calls_per_day: 30`, release profiles or credentials. A previously enrolled lane policy is not permission to replace its caps with an example's defaults.
4. Recreate **both checks and openfactory** after policy/image changes, then restart/recover. A read-only single-file checks policy mount can retain the old inode after an atomic edit; stale execution-policy handshakes fail closed. Durable delivery ownership is recovered before the engine starts; conflicting unresolved legacy ownership blocks the daemon, leaving read/resolve CLI commands available. Schema/config normalization changes can change the policy hash: previous approvals are not new authority. Omitted project concurrency and explicit `1` normalize to the same hash in this version.
5. Re-import/reapprove affected feature contracts against the new exact policy, then explicitly resume admission. Inspect the limits before permitting work.

Optional notifications: configure `webhook_url`, `webhook_secret_file`, append `deploy/compose.webhook.yaml`, and set `WEBHOOK_SECRET_FILE`. Payloads contain identifiers/status, not source/logs. Verify `X-OpenFactory-Signature` as HMAC-SHA256 of `X-OpenFactory-Timestamp + "." + rawBody`, reject old timestamps, and deduplicate `X-OpenFactory-Event-ID`. A webhook response cannot approve a release.

## Safety and limits

- Use a **dedicated VM/VPS**, not your production application host. Do not expose BB publicly; use an SSH tunnel for read-only/local access or the reviewed private HTTPS intake frontdoor above.
- Enroll **trusted repositories only**. Agents can execute code and access their model credentials. Shared workspaces are not hostile multi-tenant isolation.
- Candidate checks run on a clean checkout of the committed SHA in a separate container without controller/model credentials. Controller Git repositories and network authentication remain private.
- Delivery preserves target history: reconciliation creates a direct child of the fetched target and final integration uses an explicit expected-old-SHA lease, never an unconditional force or overwrite. Protected paths, independent review, policy hashes, and exact current-generation checks gate advancement.
- Production profiles require explicit qualification, expiry, allowed risks/paths, observation, and authorized rollback. Executables must live under operator-owned `release_dir` (`/config/releases` by default), not in candidate source. They run from a private clean snapshot; installation does not configure or perform a production deployment.
- `max_calls_per_day` and Program `max_model_calls` count controller-reserved sessions/dispatches, **not every provider API turn** inside omp's tool loop. Session/time/step bounds provide no hard token, credit, dollar or provider-request ceiling. Monitor provider usage/billing and configure provider-side limits separately.
- Default images include Go, Node/npm, Bun, Python, Git, and build tools. Add project-specific toolchains to the Dockerfile; no host Docker daemon is exposed.
- Back up SQLite **and** the BB/auth/artifact volumes securely. Do not delete volumes to recover an uncertain operation.
- Workspace/object retention is operator-managed. Monitor disk use; retained evidence and failed attempts are not silently pruned.

## Develop

```sh
go test ./...
go vet ./...
go build ./cmd/openfactory
```

Go 1.26+ and Git 2.38+. Without `check_socket`, local checks run directly with the local user's filesystem authority; Docker is the recommended isolation boundary. Pinned integrations: **BB 0.47.1**, **omp 18.10.3**. See [DESIGN.md](DESIGN.md) for state, recovery, authority, and upstream research.

### Verification boundary

**OpenFactory rename verification:** the renamed source passed the full native `go test -race ./...`, `go vet ./...`, CLI build, Linux race checks for artifacts/Store/frontdoor/Git, Compose validation and shell syntax checks. The renamed BB plugin passed typechecking, **111 tests** and its build; both Docker image targets built. The pinned omp/Bun intake and research adapters passed **16 tests / 464 assertions**.

An isolated, credential-free runtime exercised the `openfactory` CLI, fresh database and policy, standalone dashboard, and final packaged BB plugin (`id: openfactory`, display **OpenFactory**). At a 390px browser viewport the native board showed live controller data, and the Intake panel used the renamed bootstrap/session endpoints and correctly required operator passkey sign-in. No feature execution, passkey enrollment or model generation was requested by this rename smoke. It does not repeat the full historical workflow qualification below or qualify migration of an existing installation.

**Historical intake verification before the source rename, 2026-10-29 UTC:** the complete native `go test -race ./...`, `go vet ./...`, controller build, Compose validation and shell syntax checks passed. Linux race checks passed for artifacts, Store, frontdoor and Git workspaces. The BB plugin passed typechecking, **111 tests** and its build; both deployment image targets built. Pinned omp tool-adapter checks passed separately. These retained observations do not verify the renamed source or fresh namespace.

An isolated installed BB browser workflow used **deterministic semantic workers**, but real WebAuthn cryptography, controller/SQLite/CAS, public-source capture, Python execution, checks and private Git delivery. It exercised durable zero-call Save/restart, Feature delivery, question-bound Plan revision and child approval, and a complete Program with separate revision/architecture/each-experiment/product grants. One real predicate was falsified and two were supported; three product receipts reached real Git, ending at `ae0d5ec6b967d793db43786064d88fe79033ffa8`. The final fixture recorded 16 UP+UV grants, no non-UV grant, and 49 unique dispatch reservations under its explicit 120-call allowance—not the live operator's thirty-call policy.

Browser proof also covered same-request two-device CAS, partial-response/close-reopen receipt recovery without duplicate drafting, concrete capacity admission/owned cancellation, daily and lifetime waits, explicit private-bundle consent, device revocation, full auth-generation restore and unresolved old-generation intents without POST replay. An actual BB HTML attachment ran only with opaque origin `null`; cookie access and privileged fetch failed. These are emulated-browser/native-fixture observations, **not physical-phone, private Tailscale HTTPS, full real-model, vendor/GPU or production qualification**. See [DESIGN.md](DESIGN.md#intake-evidence-and-limits) for the distinct evidence categories and provider prerequisite.

The following observations are **historical discovery/parallelism-phase evidence, not proof of the new OpenFactory Intake workflow**. Observed on **2026-10-28** after discovery integration: `go test -race ./...`, `go vet ./...`, and the controller build passed. The BB plugin passed typechecking, all **23 tests**, and its build; the research extension passed **nine tests** through pinned omp/Bun runtime adapters. Both Docker image targets built, and Compose configuration and launch-script syntax checks passed.

A disposable **real engine + SQLite + private Git + isolated Docker check-service** run used an explicitly **deterministic builder/reviewer adapter**: three active features and three overlapping checks, at most one delivery owner, all three accepted, and three chained final commits containing all changes. Two candidates refreshed checks/review at generation 2; eight reserved calls stayed within the unchanged thirty-call limit.

This is **not proof of live BB/model concurrency or production/release qualification**. In that historical run an actual Docker BB spawn reached a thread but failed because no model was selected/authenticated; the configured selector was absent from its catalog, and an isolated native BB catalog exposed only Ollama. This is a historical observation, not a claim that authentication can never work. No host credentials were copied or provider fallback attempted. Subsequent operator-specific global and project concurrency changes and a controller restart are historical policy evidence, not packaged defaults or actions performed by this rename; discovery remains unrolled out.

Complete discovery smoke passed natively and inside the Linux controller image, using actual Store/Engine/Git, public research broker, private Unix runner, CAS and dashboard with **deterministic analysis/builder/reviewer adapters**, not a full real-model workflow. Eight primary-source captures and two keyless searches fed one real predicate falsification and two corrected supported experiments. Exact approvals, stale-answer rejection, two initially unapproved proposals, explicit approval through accepted Git delivery, replay deduplication, and invalidation blocking remaining authority were exercised. Delivered history retained impact findings; a separate missing-GPU case launched zero commands. Complete immutable inputs larger than the default prompt bound were read without truncation.

A separate actual **omp 18.10.3 ACP** session used the existing BB-only OpenRouter authentication and explicit Qwen model: two successful model responses invoked capture, byte-paged workspace reading, and captured-source reading. Production-authorized keyless MCP search returned ten leads. Two earlier fixture-permission responses failed, for four nonzero-usage responses total. This limited tool proof is not a full real-model discovery graph; no host authentication was copied, credentials changed, or provider fallback used.

Browser checks exercised the installed **BB Programs** list/detail, actual falsified and supported receipts, artifact coverage, and accepted versus evidence-blocked linked features. The ordinary board showed two product features while hiding fourteen capacity-accounted discovery steps. Standalone pages exposed experiment evidence and separately labeled, paginated evaluation stages. These observations do not qualify production execution or live model concurrency; the new discovery workflow has not been rolled out to the live operator.

For OpenFactory Intake, a separate narrowly scoped **actual native omp 18.10.3 ACP** probe selected `openrouter/qwen/qwen3.8-flash` with high thinking, declared only `intake_read`, and verified one SHA-bound read of a **395-byte public fixture** with a nonce. It returned strict consequential-question JSON. This used existing BB-only authentication, without host-key copying, relogin, provider fallback or default changes. It is **not** Astra/subscription evidence, a full real-model intake graph, product/check proof, physical-phone proof, or private Tailscale deployment proof. Deterministic full-graph semantic fixtures must not be confused with actual WebAuthn cryptography, native runtime/tool checks or browser/frontdoor boundaries. The live operator stack remains intentionally stopped and intake has not been rolled out.

A separate production `bb.Client` → native BB/ACP probe reached a real worker with the explicit Qwen model and native medium reasoning, but OpenRouter returned **HTTP 402** for its requested maximum output-token allowance before any tool call. The strict intake parser rejected that provider-error text; no draft or execution authority was accepted. This is a provider/token-budget prerequisite, not a successful native BB scoped-read or full real-model workflow. Review the generation allowance or provider credits before piloting that route; no billing, model fallback or token settings were changed.
