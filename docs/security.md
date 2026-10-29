# Security and trust boundaries

For operators deciding what to enroll, expose or authorize. OpenFactory is a **trusted-repository, single-operator engineering factory**, not an adversarial multi-tenant sandbox or unattended production service. Read [Operations](operations.md) for procedures and [Architecture](architecture.md) for the mechanisms behind these boundaries.

## What installation does not authorize

A fresh installation enrolls no repositories, approves no work and has no production authority. Model/provider access is operator-supplied; a selector name, successful catalog lookup or `doctor` is not proof of subscription access, authenticated generation, write permission or production readiness. No service accounts, subscriptions, private project paths or credentials are included with this [MIT-licensed repository](../LICENSE).

Feature approval binds an exact revision and operator policy. Plans and Programs publish **unapproved** product contracts. Program revision, architecture, every frozen experiment contract, product implementation, retry/resume and release authority remain separate grants. A model response, BB thread, plugin identity, dashboard, webhook response or imported draft cannot manufacture them. Protected-source changes and OpenFactory's own executable/policy updates require a separate operator decision, never automatic self-upgrade through its release lane.

## Host, workers and checks

Use a dedicated VM/VPS, not a production application host. Enroll **trusted repositories only**. Coding agents execute programs and can access their model credentials. Worktrees, tool restrictions and cleared environment variables do not make hostile code safe. A shell-capable worker can execute other programs; shared workspaces are not tenant isolation.

The Compose boundary deliberately separates authority:

| Component | Scope and limits |
| --- | --- |
| Controller | Private SQLite/Git state, enrolled Git credentials and allowlisted release authority |
| BB/omp | Dedicated model-auth home and worker workspaces; no controller database/operator-auth mount |
| Checks/experiment runner | Candidate workspaces, CAS, durable runner journal and read-only execution policy; no SQLite, Git credentials, provider home or production secrets |

There is no host-home mount, SSH-agent forwarding or Docker socket. Containers run nonroot with read-only roots except explicit volumes/tmpfs and resource/process bounds. Checks use a clean checkout of the committed candidate SHA and operator-enrolled argv, not ticket-invented shell commands. Ignored builder artifacts cannot satisfy candidate acceptance. Protected paths are checked before candidate execution and integration; review is independently performed and checked against a workspace fingerprint.

The runner is a separate trusted same-UID service, **not a hostile-code sandbox**. A fingerprint catches persistent mutation, not a process that changes and restores bytes during a check. Process-group cancellation does not contain deliberately detached processes. Local checks without `check_socket` run with the local user's filesystem authority. Container resource limits constrain the service, not independently measured per-run consumption; executable binary SHA is observed, but image identity is operator-declared, not automatically OCI-attested.

Checks have no external network route and cannot reach BB's API by default. The optional dependency-download overlay restores outbound/host-service access for trusted checks; it weakens isolation rather than making untrusted code safe. Git credentials stay in controller-private state, never checkout URLs; controller Git does not trust candidate-controlled Git configuration. See [credential setup](operations.md#configure-credentials-with-dedicated-files).

## Private browser access and human authority

BB is command-capable and must not be public. Default host publications are loopback only. The dashboard is read-only, **not an authenticated public service**; an explicit non-loopback bind does not add authentication. For remote observation use an SSH tunnel. Actual phone use requires the opt-in, same-origin private HTTPS [frontdoor procedure](phone-intake.md#prepare-private-https); Intake is disabled by default. Do not expose Funnel, raw public ingress, wildcard CORS or a direct BB attachment-route bypass.

The frontdoor confines worker-controlled BB attachments with enforcing CSP `sandbox allow-scripts` **without `allow-same-origin`**, plus `nosniff`, including errors and 304s. Attachment scripts have an opaque origin, not the approval origin. Operator cookies/CSRF headers are stripped before BB; BB-native cookies and the actual browser Origin remain. Forwarded identity headers, local-origin claims, Tailscale headers and service keys are not operator identity. BB receives no controller auth mount or bridge secret. An equivalent private proxy must preserve paths, Origin, cookie Path and streams without bypassing attachment confinement.

Passkey enrollment requires a trusted CLI, phone registration and out-of-band confirmation of the full credential fingerprint shown by that registration screen, then a fresh login. Save writes require a live scoped session, exact Origin and CSRF. Authority grants require a fresh **user-presence plus user-verification (UP+UV)** ceremony for the exact current command, content, revision, policy, bundle and evidence. The browser verifies exact canonical bytes; the execution-policy hash is only an opaque fingerprint because global host paths/argv remain private.

A passkey proves the ceremony, not that the authenticator displayed or understood the contract, that the frontend is uncompromised or that a physical phone was used. Authenticator private keys remain with the OS/authenticator. Trusted frontend integrity remains a boundary. [Enrollment and recovery instructions](phone-intake.md) intentionally live in one place.

## Disclosure and untrusted research

Phone dictation is OS-keyboard **text**, not audio upload/transcription. Save, import, export, polling and waiting do zero model work. **Generate Proposal** explicitly consents to one bounded draft over the current disclosed context, exact configured model/provider and selected bundle hashes. Send saves then generates; saving/uploading a private bundle does not grant model disclosure.

Bundles include only declared regular files, not a directory crawl. A shape-valid saved Program may keep `private_material_to_models: false`; execution approval rejects missing required disclosure instead of silently enabling consent. Approve-and-queue is an execution grant, not an import-only fallback: missing consent fails before authority or partial import. Never submit a private browser `file://` path as public research or put credentials/provider homes/secrets in search queries.

Public topics/URLs are themselves explicit disclosures. Captured public content is untrusted evidence, never executable authority. A captured quote establishes occurrence, not truth; inference, unknowns and contradictions must remain distinct. Black-box predicates establish bounded observations, not private vendor internals or GPU parity. Missing hardware/accounts, timeouts, infrastructure faults and truncated output cannot be relabeled successful observations. No account provisioning, extra hardware, provider fallback or broader spend authority is implied by research approval.

## Budgets are not billing ceilings

Global `max_calls_per_day` and Program `max_model_calls` count controller-reserved sessions/dispatches, **not every provider API turn** within omp's tool loop. Session/time/step/retry limits are not hard token, dollar, credit or provider-request ceilings. Billed-token accounting is not established by the inspected BB/omp path; context-window occupancy is not billed usage. Keep unavailable usage visibly unavailable and monitor provider-side usage/billing with separate credit/rate limits.

OpenRouter credits, API charges and subscription quotas are different resources. Use capped credentials and approved routes. Never silently switch from a subscription to API-key billing, change provider after a quota error or send a conversation elsewhere without approval. The observed [native BB HTTP 402](verification.md#native-bb-route-provider-rejection) was a failure before tools, not a successful generation or authorization to alter limits.

## Delivery and production

Checks, independent review, protected paths, exact policy and current-generation evidence gate advancement. A changed integration candidate needs fresh checks/review. Reconciliation preserves the approved source delta and target history; the final push compares the exact expected old SHA and requires a direct-parent fast-forward. Conflicts/rewritten history block rather than inviting automatic agent conflict resolution or overwrite. Unknown external outcomes retain ownership; a timeout/cancel is not proof an operation stopped.

A production profile must explicitly enroll qualified, expiring authority, allowed paths/risks, deploy/observe/rollback argv, observation duration and minimum samples. Executables live under operator-owned read-only `release_dir` (`/config/releases` by default), never candidate source, and run from a controller-private clean snapshot. No profile means no release. Observation failure, expired qualification, protected changes or uncertain effects block progress. Preflight checks feasibility before integration; installation itself deploys nothing. Rollback must be explicitly enrolled, and database changes are not automatically reversible.

## Recovery, retained data and disclosure

Back up SQLite, complete CAS and configuration together; BB/provider authentication and runner/release state need their own secure backups. Do not delete volumes or ownership records to repair uncertainty. Auth restore is mandatory **before exposing restored state or admitting restored work**; arbitrary rollback is not automatically detected. Device/operator revocation prevents future sessions/challenges/commands but does not retroactively cancel committed bounded work. Explicit cancellation and external settlement remain necessary. See [Operations](operations.md#back-up-stop-and-restore) and [Phone recovery](phone-intake.md#revoke-access-or-restore-authentication).

Keep pairing secrets/fragments, passkey assertions, cookies, CSRF, provider credentials and private transcripts out of logs, tickets, prompts and proxy logs. Browser recovery storage contains only scoped non-sensitive command/disclosure metadata, never private dictation/source/answer bodies or authentication material. Overview/webhook payloads intentionally omit source, prompts and credentials; that does not make retained local state or backups public. Evidence and failed attempts are retained until the operator manages retention.

Do not execute untrusted fork code with privileged CI secrets. Review dependency/controller changes separately from work this controller governs. Deterministic fixtures and limited real-model probes are not production, physical-phone or full-real-model qualification; consult the category-by-category [Verification record](verification.md). [Back to README](../README.md).
