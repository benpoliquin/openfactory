# Workflows

Turn a concrete change, a larger goal, or a research question into explicitly approved work. Start with [model and trusted-repository setup](getting-started.md); this page assumes the reviewed stack is running.

[← README](../README.md) · [Getting started](getting-started.md) · [Phone intake](phone-intake.md) · [Operations](operations.md)

Commands below use the public example IDs. **Replace IDs, paths, hashes and artifact identifiers with your actual returned values.** These are operator procedures, not a claim that your installation has executed the examples.

## Queue a Feature

Use a Feature when you can describe the desired behavior and observable acceptance cases.

1. Copy [examples/feature.md](../examples/feature.md) to `config/feature.md`.
2. Replace its project, ID, goal and acceptance cases with a real change. Set enrolled check names, dependencies and the intended completion target.
3. Import, inspect the returned revision, then approve that **exact** revision:

```sh
./start.sh cli enqueue /config/feature.md
./start.sh cli approve csv-quoted-fields --revision HASH_FROM_ENQUEUE
./start.sh cli status
./start.sh cli evidence csv-quoted-fields
```

`/config/feature.md` is the container path for the host's `config/feature.md`. Approval binds the immutable Feature revision **and current operator policy**. Editing a source file does not edit an approved contract; import and review the new revision. Identical imports are idempotent. Invalid identities, missing dependencies and cycles are rejected.

Choose the smallest required completion target:

| Target | Meaning |
| --- | --- |
| `reviewed` | Checked and independently reviewed, with a local receipt; no delivery lane |
| `pull_request` | A PR is published, **not integrated code** |
| `integrated` | Checked/reviewed code integrated into the enrolled branch |
| `staging` / `production` | Integration plus the explicitly enrolled release/observation gates |

`depends_on` orders work using integrated code receipts; a reviewed result or open PR does not satisfy an integrated dependency. Staging/production require independently qualified release profiles, not just a target label. See [release authority and limitations](security.md).

Follow the [board and delivery evidence](#follow-parallel-work-and-delivery). If work blocks, inspect questions and events rather than granting broader authority:

```sh
./start.sh cli questions
./start.sh cli events csv-quoted-fields
./start.sh cli evidence csv-quoted-fields
```

For pause, cancel, safe retry and unknown external outcomes, use [operations](operations.md). A timeout or cancellation is not proof that remote execution stopped.

## Turn a goal into a Plan

Use a Plan when the work needs decomposition. Copy/edit [examples/goal.md](../examples/goal.md) into `config/goal.md`, then:

```sh
./start.sh cli plan myapp /config/goal.md
./start.sh cli questions
```

This authorizes a **bounded read-only planning task**, not implementation. The model inspects current source and proposes dependency-ordered Feature contracts. Review those contracts and approve only their exact revisions using the ordinary Feature command above; proposals never approve themselves. Reuse current capabilities and evidence rather than rebuilding something merely because an old roadmap lists it as missing.

Keep your goal file within half of `max_prompt_bytes`: **12,000 bytes** with the default policy. See [runtime bounds](operations.md#review-the-namespace-before-starting) before submitting a larger goal.

The [phone Plan route](phone-intake.md#save-generate-and-approve) can present consequential questions. Its answers bind the question's revision, attempt and output; an answer creates a **new unapproved planning revision** requiring review. Its published child Features remain unapproved too. Waiting for questions or polling progress makes no model calls.

## Run a Discovery Program

Use a Program to research an objective, test hypotheses and propose **unapproved implementation contracts**. It is not delivered code. See [verification and known limits](verification.md) before piloting.

### Prepare inputs and disclosure

Review these related examples together:

- [Program specification](../examples/program.json): objective, project, scope/non-goals, public sources, provider/search/private-material disclosure and lifetime bounds.
- [Bundle manifest](../examples/program.bundle.json) and [experiment.py](../examples/experiment.py): explicitly declared regular files only, not a directory crawl.
- [Experiment profiles](../examples/experiment.profile.yaml): reviewed argv profiles and permitted arguments, not arbitrary model shell commands.

Enroll the example's `discovery-fixture` project with real checks, or consistently replace that project in your inputs. Merge only reviewed execution profiles into **stopped** operator configuration using the [policy upgrade procedure](operations.md#upgrade-policy-images-or-lane-limits). Copy `program.json`, `program.bundle.json` and `experiment.py` into `config/` together; the manifest resolves declared files relative to itself. The example explicitly consents to sending its script to the configured provider. Match `model_provider_ids` to your actual approved route; do not assume the example disclosure authorizes another provider.

For public-only research, remove the bundle seed from the explicit Program JSON, set `disclosure.private_material_to_models` to `false`, and import **without `--bundle`**. Public URLs/search topics are explicit disclosures too; never put secrets in searches or present a private browser `file://` path as a public source.

Uploading a private bundle or importing a proposal performs shape validation/storage with **zero model calls**; it is **not model consent** and can retain `private_material_to_models: false`. Research/execution approval remains strict: missing required context, provider or search consent is rejected, never silently enabled. Research starts without a product clone and uses a restricted discovery tool profile; public content is evidence input, not execution authority.

### Import and approve the exact revision

```sh
./start.sh cli program import /config/program.json --bundle /config/program.bundle.json
./start.sh cli program approve event-ordering-discovery --revision REVISION_HASH_FROM_IMPORT
./start.sh cli program status event-ordering-discovery
./start.sh cli program questions event-ordering-discovery
```

Review the imported objective, disclosure and limits before approving. Programs share the **same scheduler, project/global lanes and daily-call reservations** as Features and Intake; there is no second scheduler. Lifetime call, iteration, step, experiment, search, source and captured-byte bounds survive answers/reimports. Controller dispatch counts are not hard provider-turn/token/credit/dollar limits.

### Answer consequential questions

Create `config/answer.json` as a strict `ProgramAnswer` JSON object containing `question_id` and `text`, using the actual current question ID and your answer. Then:

```sh
./start.sh cli program answer event-ordering-discovery --revision EXACT_CURRENT_REVISION_HASH --file /config/answer.json
./start.sh cli program approve event-ordering-discovery --revision NEW_REVISION_HASH
```

The answer command binds operator identity and the exact question revision. **Inspect the new revision before approving it.** Stale answers fail instead of silently answering a newer question. Questions block without repeated model calls; missing hardware/accounts/prerequisites remain waits, not fabricated observations.

### Review architecture and every experiment

Inspect the architecture and its evidence before granting its exact content authority:

```sh
./start.sh cli program architecture event-ordering-discovery
./start.sh cli program evidence event-ordering-discovery --artifact ARTIFACT_ID
./start.sh cli program approve-architecture event-ordering-discovery --revision EXACT_CURRENT_PROGRAM_REV_HASH --artifact ARCHITECTURE_CONTENT_SHA
```

Inspect the frozen experiments and approve **each** intended contract separately:

```sh
./start.sh cli program experiments event-ordering-discovery
./start.sh cli program approve-experiment event-ordering-discovery --revision EXACT_CURRENT_PROGRAM_REV_HASH --contract EXPERIMENT_CONTRACT_SHA
./start.sh cli program evidence event-ordering-discovery
```

Repeat exact-content architecture/experiment grants on every materially revised iteration; Program approval is not a blanket experiment grant. Experiment execution binds enrolled profiles, frozen bundle, bounds and execution-policy SHA. Missing prerequisites, infrastructure faults, timeouts or truncated output do not establish success or falsification; complete typed observations and predicates are required. A nonzero exit alone is not falsification, and a failing predicate can falsify with exit zero.

Inspect independent evaluations, actual receipts, scope and contradictions before accepting conclusions. Supported results are bounded observations, not proof of private vendor internals, hardware parity or production readiness. For settled failed steps, the [phone Review resume/retry procedure](phone-intake.md#recover-a-settled-program-step) requires a distinct fresh grant and preserves attempt/lifetime caps; it does not rerun conclusive results or clear uncertain ownership.

### Approve proposed product work separately

```sh
./start.sh cli program features event-ordering-discovery
./start.sh cli approve PROPOSED_FEATURE_ID --revision EXACT_FEATURE_REVISION_HASH
```

Only approve after inspecting the actual proposal and its evidence. Program, architecture and experiment approvals **never approve the product Features**. Generated Features retain evidence gates as well as ordinary checks, independent review and authorized delivery. Evidence invalidation blocks remaining authority and records impact findings for already delivered work; it does not erase history or automatically undo delivery.

Read-only views are `/programs`, `/programs/ID`, `/api/programs` and `/api/programs/ID` on the dashboard, plus **Programs** in BB's OpenFactory sidebar. Lists paginate. Questions, architecture, experiments, evidence, features and events remain inspectable; history/current-stage labels show provenance, **not approval authority**. See [architecture](architecture.md) for evidence identity and [verification](verification.md) for the limits of real-model and deterministic qualification.

## Follow parallel work and delivery

The ordinary controller/SQLite scheduler owns **all** Feature, Program and Intake lanes. Public defaults are:

| Limit | Default |
| --- | ---: |
| Global active attempts, `max_concurrent` | **2** |
| Each project's `max_concurrent` | **1** (allowed **1..32**) |
| Global `max_calls_per_day` | **30 reservations** |

**Opt-in illustration:** setting one project's `max_concurrent: 2` allows its two independent Features to build, check and receive independent review in separate attempt workspaces, only if the reviewed **global cap also has capacity**. More lanes do not approve work, increase daily budgets, bypass dependencies or grant release authority. Use the [coordinated policy upgrade procedure](operations.md#upgrade-policy-images-or-lane-limits) to change caps; private operator settings are not public defaults.

Delivery is **serialized in a durable FIFO lane per canonical repository and target branch**, including differently named projects and HTTPS/SSH aliases. Local path/file aliases resolve symlinks. Staging/production also hold a repository/environment resource. Unrelated builds and repositories can continue while one delivery lane is busy.

When the target moves, the controller applies the immutable original Feature delta onto the fetched head in a private workspace. A changed candidate receives a new generation and **fresh configured checks plus independent review** before delivery. Conflicts or rewritten history block without automatic agent conflict resolution or remote writes. The final push requires the exact expected old SHA and a direct-parent fast-forward candidate. Unknown spawn/push/deploy outcomes retain ownership and capacity until reconciled; another lane cannot guess or overwrite them.

Open the read-only [dashboard](http://127.0.0.1:38887) or [BB board](http://127.0.0.1:38886/plugins/openfactory/board). Lanes show queued → building → checking → reviewing → waiting to merge → integrated, alongside actual stages, dependency/capacity waits, candidate generation, checks/review, session links and PRs. An open PR stays **waiting to merge**; review-only work stays **reviewed**. Old-generation evidence cannot prove the current candidate.

Use search/project filters and expand **Recorded history** for parallel work and waits on a shared timeline. Stripes distinguish waiting from work; open intervals stop at snapshot time, with no forecast ETA. Older records show only recoverable history. The board refreshes every four seconds while visible and reports stale snapshots/upstream errors instead of inventing progress. Dashboard/board polling makes no model calls and cannot enqueue, approve, cancel, retry or deploy. Opt-in [Intake](phone-intake.md) is the separate authenticated approval interface.
