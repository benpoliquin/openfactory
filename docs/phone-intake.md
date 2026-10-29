# Private phone intake

Dictate a request, save it durably, generate a bounded proposal, then approve exact reviewed work with a passkey. This is an **opt-in operator deployment**, disabled by default—not BB's native worker-chat composer or an audio/transcription service. Phone dictation is the OS keyboard entering **text**.

[← README](../README.md) · [Getting started](getting-started.md) · [Workflows](workflows.md) · [Operations](operations.md) · [Security](security.md)

Before piloting, read [verification](verification.md), including the [native BB HTTP 402 prerequisite](verification.md#native-bb-route-provider-rejection). Historical browser/native fixtures are **not physical-phone, private Tailscale HTTPS, full real-model or production qualification**.

## Prepare private HTTPS

Start with an enrolled trusted project and configured models from [getting started](getting-started.md). Use the complete [private HTTPS deployment recipe](../examples/intake-private-https.txt) and [policy template](../examples/openfactory.yaml); `start.sh` does **not** automatically deploy that recipe.

### Prepare the stopped stack

Follow [pause, drain, stop and coordinated policy upgrade](operations.md#upgrade-policy-images-or-lane-limits), including [backup](operations.md#back-up-stop-and-restore). Back up **SQLite, the complete artifact CAS and operator configuration together**; preserve BB/provider state by its existing procedure. Schema migration is forward-only: an older binary must not open schema 4. Policy/image changes must recreate **both controller and checker**; atomic replacement of the policy can leave the checker's single-file mount on an old inode.

### Set one exact browser identity

Choose your actual stable private HTTPS hostname. In reviewed `config/openfactory.yaml`, enable Intake with the same hostname everywhere:

```yaml
intake:
  enabled: true
  external_origin: https://openfactory.example-tailnet.ts.net
  rp_id: openfactory.example-tailnet.ts.net
  public_path: /openfactory-intake
```

**Replace the example hostname.** `external_origin` must exactly match browser serialization: lowercase HTTPS scheme/hostname, no trailing slash, credentials, path, query, fragment, explicit default `:443` or zero-padded port. RP ID is the **exact hostname**, without scheme/port—not a parent-domain shortcut. Changing RP ID breaks existing passkey associations and requires planned re-enrollment, not an authentication bypass.

Inspect effective nonsecret policy and its hash:

```sh
./start.sh cli config intake
```

In reviewed `.env`, set `OPENFACTORY_BB_APP_URL=https://openfactory.example-tailnet.ts.net`. Compose passes this to BB as `BB_APP_URL`. **Persisted BB configuration takes precedence** over environment, so inspect/update the intended installation explicitly when necessary with the pinned BB CLI:

```sh
./start.sh bb bb-app config set BB_APP_URL https://openfactory.example-tailnet.ts.net
./start.sh bb bb-app config list
```

These commands deliberately modify/list BB configuration; use only for the intended installation. `BB_SERVER_URL` selects internal CLI/host transport, **not** the browser-origin allowlist.

For **local Docker BB only**, explicitly set `OPENFACTORY_BB_HOST_AUTHORITY=localhost:38886` if required by that BB's accepted HTTP authorities (equivalent reviewed YAML: top-level `bb_host_authority: localhost:38886`). The controller still connects to `bb_url`, normally `http://bb:38886`; this override changes **only HTTP Host**, never connection DNS, URL/path, TLS SNI or certificate identity. Its empty default preserves remote HTTPS virtual hosts. Never automatically apply localhost to remote BB or substitute a static Docker IP. Review [the pinned BB configuration source](https://github.com/get-bb/bb/blob/5e1acd765039e874643f34c4d35409e58484e408/docs/configuration.md) for precedence and accepted authorities.

Complete the coordinated recreation/recovery before permitting work. `doctor` reports the URL/Host mode but does not prove provider billing, successful model generation or production readiness.

### Send all paths through the frontdoor

Review existing Tailscale Serve mounts and tailnet ACLs, then use one private root upstream:

```sh
tailscale serve status
tailscale serve --bg --https=443 http://127.0.0.1:38889
tailscale serve status
```

Do **not** reset unrelated apps or use Funnel/public ingress. Ensure a more-specific mount cannot bypass the frontdoor. Forward **all HTTPS paths intact** to `38889`, not root to BB plus a separate API mount; do not strip `/openfactory-intake` at Tailscale. The frontdoor strips that prefix internally into authenticated `/v1`; bare `/v1/*` is not public. Other paths proxy to BB with native WebSocket/SSE support. See [Tailscale Serve reference](https://tailscale.com/docs/reference/tailscale-cli/serve).

The enabled frontdoor is part of the existing controller process, not another app, scheduler or daemon. Native default is `127.0.0.1:38889`; `--command-listen IP:PORT` and `--command-allow-non-loopback` control explicit binds. Compose's non-loopback bind is **inside Docker**; ports `38886`, `38887` and optional `38889` remain published on **host loopback only**. Disabled Intake opens no frontdoor socket.

Open your private HTTPS origin in the phone browser, then **OpenFactory Intake** in BB's sidebar at `/plugins/openfactory/intake/`. Private browsers must use the frontdoor, **never direct BB 38886**. The frontdoor also isolates worker-controlled attachments from the approval origin; see [Security](security.md).

BB sign-in and forwarded identity headers do not grant operator approval. An equivalent private proxy must preserve **all paths, Origin, cookie Path, WebSocket/SSE streams and attachment confinement**, without wildcard CORS or route bypasses. Keep fragments, pairing bodies, assertions, cookies, CSRF and private transcripts out of logs.

Exact `http://localhost:PORT` with RP ID `localhost` is an **isolated-development exception**, not a phone recipe. Remote raw-IP HTTP, TLS-warning bypasses, split frontend/API origins and disabling user verification are unsupported.

## Pair, confirm and log in

Run from a **trusted local terminal**, never a BB thread, model prompt, log or ticket. The example uses operator `owner` and project `myapp`; replace them with your intended operator and enrolled project:

```sh
./start.sh cli operator create owner --label 'OpenFactory owner' --project myapp
./start.sh cli operator status owner
./start.sh cli operator pair owner --output /tmp/owner-pairing.json
```

Repeat `--project` for each explicitly authorized enrolled project. Pairing uses a random 256-bit secret in the URL **fragment**, never the query, with a default ten-minute lifetime. The output file is created exclusively with mode `0600`; Compose paths are **inside the controller container**. Read it only in your trusted terminal:

```sh
./start.sh compose exec -T openfactory cat /tmp/owner-pairing.json
```

Delete the secret-bearing file after pairing. Omitting `--output` prints secret-bearing JSON to the terminal; do not send it to models or logs. Expired links require a new pairing.

Open the link on the phone at the configured private origin and register a passkey. Read the **full lowercase SHA-256 credential fingerprint from that phone's successful registration screen only**. Substitute that value and the returned pairing ID below:

```sh
./start.sh cli operator pairing PAIRING_ID
./start.sh cli operator confirm PAIRING_ID --fingerprint FULL_LOWERCASE_SHA256
./start.sh cli operator devices owner
```

Pairing inspection never echoes the pending fingerprint or a confirmation code; it cannot be your fingerprint source. A mismatch **revokes that pairing**: issue a new pairing and repeat registration. Confirmation atomically activates exactly that credential and consumes the pairing.

**Registration is not login. Perform a fresh passkey login afterward.** Private keys stay in the OS/authenticator, not BB/controller storage. Passkeys prove user presence and verification, not that an authenticator displayed or understood contract text; frontend/browser integrity remains a trust boundary. Each execution grant requires another fresh ceremony.

## Save, generate and approve

1. **Select the enrolled project and dictate/type. Save** persists the exact request/turn with **zero model work** and no model-disclosure requirement. Project identity is immutable; switching projects creates a new linked request.
2. **Generate Proposal** explicitly authorizes one bounded read-only draft of the **current saved context**, exact configured provider/model selectors and selected bundle hashes shown in the server disclosure. **Send** composes Save then Generate with separate stable command receipts. Save, uploads and imports are not model consent; private material may remain saved without sending it to a model. There is no browser-selected arbitrary provider or silent fallback.
3. **Read questions and refine.** Missing resources and consequential decisions block instead of inventing results. Answer the bound question and explicitly generate again as needed. Polling status/history or waiting makes no model calls.
4. **Inspect the actual review card:** exact canonical Feature Markdown, read-only Plan or Program JSON, source-byte SHA, domain revision, findings, assumptions, risks and selected evidence/bundle. Source edits invalidate the old review. Existing canonical formats support import/edit/export; shape-valid unapproved drafts, including private Program bundles without consent, can remain saved/exportable with zero model use.
5. **Approve and queue** only the exact reviewed card with a fresh **user-presence + user-verification** passkey ceremony. This is an execution grant, **not import-only fallback**: missing required disclosure rejects before authority or partial import, leaving the draft editable. The grant binds current request/content/revision, typed command, policy, bundle and evidence—not a model summary.
6. **Follow durable status and recorded BB worker links.** `submitted` means handoff committed, **not delivered**. Ordinary Feature checks, independent review and delivery gates remain authoritative.

The existing board and Programs views remain read-only; Intake sends authenticated commands directly to the same-origin controller, not through BB mutation RPC, native worker chat, a bridge key or a model-created approval.

### Understand capacity and downstream grants

Drafting starts in an empty scoped workspace without a product clone. Only restricted `intake_read` can read immutable bound context; it grants no arbitrary tools or second scheduler. Defaults remain **2 global active attempts, 1 per project and 30 daily reservations**, shared with Features/Programs. The default request lifetime cap is **12 draft dispatches**, frozen at creation and not reset by edits, retry, reconnect or restart. Bounds count dispatches, not a hard provider-turn/token/dollar ceiling; private operator lane settings are not public defaults.

Queued drafts report the ordinary scheduler's concrete capacity/admission wait. Creating a step is not a running model call: only successful Claim binds its attempt and publishes `drafting`. Daily-call waits and their clearing follow the owned attempt; polling cannot bypass admission or charge another dispatch. Saved work survives waits.

One request has one active draft. Superseded/uncertain ownership must settle through cancellation/reconciliation, not blind replay. Cancellation is not proof remote execution stopped.

Plans publish **unapproved child Features**; answers create a new unapproved planning revision. Program answers also create a new revision. Feature queueing, Program revision, architecture, **each frozen experiment contract**, retry/resume and final product approval are separate grants. None grants broader accounts, hardware, spend or production permission. Follow the [full Program lifecycle and CLI commands](workflows.md#run-a-discovery-program).

### Recover a settled Program step

For a failed Program worker, **Review resume** selects at most one current, settled failed step. Inspect its immutable input, internal Feature revision, latest attempt and unchanged attempt cap, then grant fresh passkey approval. Changed/uncertain ownership, cancelled Programs and exhausted lifetime/attempt allowances reject recovery.

An experiment retry retains the exact frozen contract, bundle and execution-policy binding. Supported/falsified conclusive results are not failures to rerun. `max_experiments` counts logical steps; physical retries still require fresh exact grants and ordinary attempt/per-run limits. Do not infer a never-started launch merely from a missing receipt after an intent. For uncertain external work, use [explicit reconciliation](operations.md) before retry/resume.

### Inspect or export exact source

Trusted CLI inspection does not grant execution authority:

```sh
./start.sh cli intake status
./start.sh cli intake status --limit 50 --after LAST_RETURNED_REQUEST_ID
./start.sh cli intake status REQUEST_ID
./start.sh cli intake export REQUEST_ID --revision 1 --output /tmp/draft.md
```

Replace IDs/revision with actual returned values. Follow returned `next_cursor` as `--after` while `has_more`; lists are bounded, not silently complete. Omitted `--limit` uses `history_default` (50); valid limits are 1..`history_max` (200 by default). Do not combine list options with a detail request ID.

Export writes **exact immutable source bytes**, without newline insertion/rewriting; source SHA is separate from domain revision. Use `.json` for Program drafts. Without `--output`, stdout is raw source, not JSON. Files are exclusive mode `0600`; existing files/symlinks are not overwritten.

## Reconnect without duplicating work

The browser journals pending command IDs, Send/version fences and exact non-sensitive `intake.draft` disclosure metadata, scoped to **origin, installation, auth generation and actor**. Private dictation, canonical source, answer bodies, cookies, CSRF and passkey material are **not localStorage recovery data**.

After a lost response, reconcile its durable receipt and retry **only the identical command with the same stable UUID**; never create a fresh ID to guess whether it ran. Changed content under an old ID is rejected. An installation/generation mismatch stays **unresolved**, never automatically replayed. A normal process restart preserves auth generation.

Inspect status and the saved transcript before issuing genuinely new work. Device concurrency can cause stale-content rejection rather than merging conflicting edits. Uncertain drafts retain ownership until settled; use [operations](operations.md) for external reconciliation, not deletion of ownership records.

## Revoke access or restore authentication

Use a trusted local terminal. The following are **separate choices**, not a routine sequence:

| Need | Command |
| --- | --- |
| Revoke one device | `./start.sh cli operator revoke-device CREDENTIAL_ID` |
| Revoke the operator | `./start.sh cli operator revoke owner` |
| Inspect current auth generation | `./start.sh cli operator auth-generation` |
| Invalidate all browser auth after restore | `./start.sh cli operator restore-auth --confirm invalidate-all-browser-auth` |

Replace `CREDENTIAL_ID`/`owner` with the intended identity. Revocation blocks future sessions/challenges/commands; it **does not silently cancel already committed bounded drafts or historically approved budgeted work**. Stop that work through explicit cancellation and ownership settlement.

**Run `restore-auth` before exposing any restored database or admitting restored work**, then reconcile external attempt/experiment ownership before retry/resume. Restore rotates auth generation and revokes **all old credentials, sessions, challenges and pairings**. Operators and historical work remain, but old passkeys cannot freshly log in: repeat [trusted-CLI pairing, phone registration, fingerprint confirmation and fresh login](#pair-confirm-and-log-in).

Arbitrary SQLite/whole-volume rollback is **not automatically detected**; there is no external epoch-file guarantee. Never recover through unauthenticated endpoints, deletion of ownership records or database-only restore that loses CAS/configuration. Keep the [complete backup procedure](operations.md#back-up-stop-and-restore) and external reconciliation part of recovery.
