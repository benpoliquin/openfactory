# Getting started

Start an empty OpenFactory stack, connect your own model access, and enroll one trusted repository. This guide is for a new Docker Compose installation; existing operators should first review [configuration and upgrades](operations.md).

[← README](../README.md) · [Workflows](workflows.md) · [Private phone intake](phone-intake.md) · [Operations](operations.md)

## Check prerequisites

- Docker with Compose **2.24.4+**, outbound internet, and your own provider credentials/credits and available model IDs.
- A dedicated VM/VPS or development host, **not your production application host**. Enroll trusted repositories only: workers execute repository code, and workspaces are not hostile multi-tenant isolation.
- **Git 2.38+** for a local controller (`merge-tree --write-tree`); the container image enforces this minimum. Local development also requires Go 1.26+; see [development](operations.md).

Review the [security boundaries](security.md) before granting repository or release access. Installation provides no model subscription, repository permission, or production qualification.

## Start an empty stack

For a fresh checkout:

```sh
git clone https://github.com/benpoliquin/openfactory.git
cd openfactory
./start.sh
```

Before launching an existing checkout, review `.env`, `COMPOSE_PROJECT_NAME` and `config/openfactory.yaml`. Existing environment values can select another namespace; the defaults do not migrate or adopt existing state. Do not implicitly reuse an operator's volumes, BB home or private configuration.

`start.sh` creates `.env` and `config/openfactory.yaml` **only if missing**, builds images, and starts the three Compose services. It does not overwrite existing operator files. The default `projects: {}` enrolls no repositories, approves no work, and makes no model calls while idle.

Open these host-loopback URLs:

| Page | URL | What it does |
| --- | --- | --- |
| Dashboard | http://127.0.0.1:38887 | Read-only controller status and evidence |
| BB | http://127.0.0.1:38886 | Agent sessions and the native plugin sidebar |
| OpenFactory board | http://127.0.0.1:38886/plugins/openfactory/board | Read-only feature lanes and recorded history |

Keep these ports private. Use an SSH tunnel for remote read-only access; actual phone intake requires the [private HTTPS frontdoor](phone-intake.md#prepare-private-https), not a direct BB exposure.

## Connect your models

Authenticate in BB, the bundled agent-session web app, then inspect its actual model catalog:

```sh
./start.sh bb omp login
./start.sh bb omp models ls --json
```

Edit `.env`:

- Set `OPENFACTORY_ASTRA_MODEL` to the **exact available Codex Astra selector** returned for your account. It is required for an enrolled repository and is used for planning, building and independent review.
- Optionally set `OPENFACTORY_FAST_MODEL` to an explicitly approved OpenRouter model, using the [OpenRouter credential overlay](operations.md#optional-openrouter). Setting it adds a read-only scout dispatch, using one extra daily reservation, before every ordinary Feature build attempt. Leave it empty to skip scouting.

Astra is a configured selector, not a guarantee of subscription access or billing treatment. A model name or successful catalog listing does not prove authentication, credits, or usable output allowances. There is no automatic provider/billing fallback after a quota error. Credentials stay in the dedicated BB volume; do not copy your host home or authentication into the controller.

The native BB provider route has a [known HTTP 402 qualification limit](verification.md#native-bb-route-provider-rejection): reaching a worker did not establish successful drafting. Review that evidence before piloting the route; do not treat login or `doctor` as full real-model qualification.

## Enroll a trusted repository

Edit `config/openfactory.yaml` using the commented project in [the public policy template](../examples/openfactory.yaml). Replace `projects: {}` with your reviewed enrollment; the commented example is **not** a runnable fake application.

For each project, explicitly choose:

1. A stable project ID (the examples use `myapp`), repository URL and target branch.
2. `trusted: true` only for code you trust to execute.
3. The correct Git auth mode and repository identity. Follow the [dedicated token/SSH credential procedure](operations.md#configure-credentials-with-dedicated-files); PR creation also needs an API token even when Git uses SSH.
4. Real named `unit` and `smoke` **argv** check profiles for that repository, including timeouts. Match the names your Feature contracts will request. Replace the example Go/smoke commands if they are not your application's actual checks.
5. Appropriate protected paths. Do not copy an example's qualified production release profile: release authority requires independently qualified, explicitly enrolled operator commands.

Defaults are **2 global active attempts**, **1 per project** (valid project range **1..32**) and **30 daily call reservations**. Those reservations count controller sessions/dispatches, not each provider API turn or a hard token/dollar ceiling. Keep provider-side limits too. Higher project concurrency is an [explicit opt-in](workflows.md#follow-parallel-work-and-delivery), not a reason to inflate the daily budget.

Checks run from a clean committed checkout in a separate container, without controller/model credentials and **without an external network route by default**. Bake dependencies/toolchains into the check image. If trusted checks truly need downloads, the [check-network overlay](operations.md) is an explicit opt-in that restores outbound/host-service access; it is not a hostile-code sandbox. No host Docker socket is forwarded.

## Confirm readiness

Apply the reviewed fresh configuration and inspect readiness:

```sh
./start.sh
./start.sh cli doctor
./start.sh cli status
```

For a stack with existing work, use the [pause, drain, stop, backup and coordinated recreation procedure](operations.md#upgrade-policy-images-or-lane-limits) instead of changing a live policy. Policy/image changes must recreate **both checker and controller**; restarting only one can leave the checker bound to an old policy inode. Old policy approvals do not silently become authority under a new policy.

`doctor` checks Git capabilities, BB/provider/model availability and configured file presence. It does **not** prove model authentication, Git write access, billed-token accounting or production readiness. Resolve its findings and review the actual model/repository/provider prerequisites before approving work.

Next: [queue a small real Feature](workflows.md#queue-a-feature). Larger goals can become unapproved Plans or [Discovery Programs](workflows.md#run-a-discovery-program). Phone dictation and passkey approval are separately [opt-in](phone-intake.md), not enabled by setup.
