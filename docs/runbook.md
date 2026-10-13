# Local MVP Runbook

This guide runs the control service on one trusted local machine. The current
macOS deployment uses the `ai-workflow` Colima profile, with PostgreSQL and the
trusted control process running as Docker Compose services. Compose publishes
8080 and 8081 only on host loopback. On Linux, use a local Docker Engine and
Docker Compose. The dashboard is on port 8081. Port 8080 serves only
`/healthz`, `/readyz`, and `POST /webhooks/github`; the optional tunnel
exposes only that webhook route. Keep GitHub App credentials, model credentials,
and the dashboard login token in the local `secrets/` directory; do not paste
them into chat or commit them.

## Start and sign in

From the repository root, run:

```sh
scripts/up.sh
```

The script creates `secrets/config.yaml` from the example when needed,
generates local database credentials, builds the control/runner images, and
starts PostgreSQL and the control service. It also creates the local login
credential at `secrets/dashboard-token`; the token is stored in that file and
is not printed as chat content. Open `http://127.0.0.1:8081` and enter the
file's contents in the dashboard login form.

The main service command is `control -config /secrets/config.yaml`. The optional
diagnostic mode is `control -doctor -config /secrets/config.yaml`; it checks the
database, reports workflow and budget counts, checks whether configured GitHub
App credentials are readable, and probes the Dev model when model settings are
complete. The web **Run diagnostics** action has a different scope: it pings the
database, queries GitHub App installations, probes the configured Dev model, and
checks Docker and the runner image. The web diagnostic skips the model probe in
Fake mode. CLI doctor does not use runner mode as a skip condition: if all model
settings are present, it makes a small Dev-model request even when Fake mode is
selected. Both skip that request when the endpoint, token, Dev model, or Review
model is missing.

## Configure the model service

Open **Settings** in the dashboard. Set the Responses-compatible API Base URL,
Dev model name, Review model name, output-token limit, and stream setting. Enter
the API token only in the password field and save. Settings persist the changed
configuration locally; a submitted token is stored in the configured secret
file. The browser does not read back or display a saved token. Leave the token
field empty to keep the current secret unchanged.

The service uses the Responses API. It does not choose a hard-coded model or
fall back to Chat Completions. Save all model fields before asking doctor to
verify a real provider. Running a real-model evaluation requires a working
endpoint, token, Dev model, and Review model. Until then, a doctor result that
reports an unconfigured model is expected, and no model request should be sent.

For a deterministic local walkthrough, select **Fake** mode and create the
local demo. The fixture's first Issue is `Repair Add`: its test is intentionally
red because `Add` subtracts. Fake mode deterministically applies the accepted
repair flow and stops at `AWAITING_MAINTAINER`. It does not call GitHub or a
real model. A demonstration run is not evidence that real-model quality or
GitHub delivery has passed acceptance.

## GitHub App setup and webhook tunnel

Sign in to the dashboard first, then use the **GitHub App registration** button
or open `http://127.0.0.1:8081/setup`. An unauthenticated request to `/setup`
redirects to the dashboard login. The manifest flow creates the App and returns to
the local callback. Complete installation and repository authorization, then
save the App ID, installation ID, private key, webhook secret, and selected
repository in local configuration. Do not expose port 8081 publicly.

When GitHub must reach a machine behind NAT, start the webhook tunnel in a
separate terminal:

```sh
scripts/tunnel.sh
```

The launcher supervises the local allowlist proxy and cloudflared together.
If the proxy exits, it is restarted without replacing the running tunnel URL.
A port owned by an unrelated process causes startup to fail clearly. Stopping
the supervisor stops its managed pair. For a background local run, create the
data directory and launch with a log file:

```sh
mkdir -p data
nohup scripts/tunnel.sh >data/tunnel-supervisor.log 2>&1 </dev/null &
```

The tunnel targets the local allowlist proxy. It forwards only `POST
/webhooks/github` to the control service. Dashboard, health, settings, and App
setup endpoints are not tunneled. Put the printed HTTPS URL plus
`/webhooks/github` into the GitHub App webhook configuration. Verify the
delivery signature and a real test delivery before treating integration as
ready. This Cloudflare quick-tunnel URL is temporary and may change when the
tunnel process restarts; update the GitHub App webhook URL after each change.
For a fixed-domain server deployment, configure that domain as the public URL
and set the App webhook endpoint to `https://<domain>/webhooks/github`.

## Automatic requirement approval

AI Triage approves a valid `accepted` TaskSpec and automatically enters
`PLANNING` by default. The approval identity is `ai:dev-triage`; an atomic
`ai_spec_approved` audit event records the generation, specification/source
digests and acceptance criteria. Both real and Fake modes use this rule.

The execution settings expose two independent gates, both defaulting to false:
`runner.require_accept_for_analysis` waits for `/ai accept` before analysis;
`runner.require_accept_for_development` waits for `/ai accept` after AI has
agreed to execute the request. The second gate keeps a proposal in `SPEC_READY`;
manual acceptance then starts development. Turning it off releases current
valid ACTIVE proposals; startup also resumes these when automatic approval is
enabled. Paused/cancelled workflows and stale proposals remain unchanged.

The normal accepted path runs analysis, development, verification, Review and
Skeptic automatically and stops at human merge. Requests for clarification,
rejection/defer decisions and execution failures remain visible exceptions.
The approval policy does not change isolation, budgets, protected paths or
GitHub merge rules.

## GitHub state labels

The service reserves the `ai-workflow:` label prefix. It synchronizes a current
phase label on the source Issue and its PR, such as `ai-workflow:需求分析`,
`ai-workflow:开发中`, `ai-workflow:审查中`, or `ai-workflow:待维护者合并`.
Paused, cancelled, human-owned and completed workflows also receive a control
label (for example `ai-workflow:已取消`). Active workflows do not get a
“running” label merely because they are eligible to run.

State changes queue label updates transactionally. A separate retrying worker
reads the latest state and replaces only labels under this reserved prefix;
other repository labels are preserved. Labels are informational and do not
change approval or merge rules. Startup and five-minute reconciliation backfill
all existing GitHub workflows, including cancelled and merged ones, and restore
removed labels. The App needs Issues write access (also used for PR labels).
Label retries do not consume automatic workflow capacity or block publication
and gate actions. Errors and receipts appear as `labels` outbox events.

## Control and merge

Use the dashboard to inspect workflow phase, control state, event audit, task
reports, evidence, model usage, and review coverage. Pause or take over a
workflow when review evidence is incomplete or uncertain. When an ACTIVE workflow
asks for clarification (`NEEDS_INFO`), an authorized human Maintainer can reply
directly on the Issue; no mention or extra command is needed to restart analysis. An accepted AI
result then follows the configured development acceptance gate.
Replies never approve a proposed specification, unpause a workflow, or take it
back from human ownership. Approve a resulting TaskSpec explicitly with `/ai accept`. The service uses a
durable PostgreSQL database and `/data` volume, with a webhook Inbox and an
external-effect Outbox for restart recovery and reconciliation. Inbox/Outbox
processing is at least once; uncertain GitHub writes require reconciliation.
At startup and every 60 seconds, the service also discovers open Issues from each
configured repository. Each pass reads the newest page and one rotating historical
page (100 records per page), excludes pull requests, and creates missing Issues
according to the intake policy. By default new Issues enter TRIAGE automatically.
Enable **Require acceptance before analysis** in Budget and execution settings to
keep them in INTAKE until a Maintainer issues `/ai accept`. Neither policy approves
development specifications or replays old comment commands. Recovery and
discovery errors appear in audit.

MVP worker settings allow up to two builders and two reviewers. Work remains
bound to a generation, lease, path scope, and verification identity. A passing
gate publishes a check for the current candidate but never merges it. A
Maintainer must inspect the pull request and merge through GitHub's normal
interface.

Configure **Concurrent workflows** in Settings → Budget and execution (1–4,
default 2). Only workflows with a valid execution lease in an automatic processing
phase consume this capacity. Waiting for requirement clarification, specification
approval, or Maintainer merge does not consume a slot. Each Issue has its own
branch, PR, sandbox, verification identity and evidence. Lowering the limit lets
current tasks finish and delays new admissions until usage drops below the limit.
Builder/reviewer settings bound the role pools across concurrent workflows.

Dashboard scheduling metadata distinguishes queued work from leased execution.
A passing PR stays available for human review while another Issue can run. When
a merge changes the target branch, affected candidates must be reverified using
the new base/head identity; paused and human-owned workflows remain under their
existing control state. Workflow concurrency never enables automatic merge.

A Maintainer may issue `/ai forceaccept` (also available in the workflow Web
actions) to force an Issue into development planning or to allow its current PR
through this system's merge gate. The forced decision is attributed and tied to
the current candidate identity; verification and review evidence remain truthful.
An explicit forceaccept may restore a settled cancellation; it waits for any
in-progress stop or external write to settle. Ordinary replies do not restore
cancelled work. It does not merge the PR or alter GitHub's other required checks. Changing the
base, head, specification or policy invalidates the current forced approval.

## Backup, restore, and shutdown

Create a database and `/data` archive with:

```sh
scripts/backup.sh [backup-root]
```

The script writes `postgres.dump` and `data.tar.gz` under a timestamped
directory. Copy that directory to protected storage and record its timestamp.
Restore requires the explicit destructive acknowledgement:

```sh
scripts/restore.sh --yes data/backups/20261012T115845Z
```

The default backup root is `data/backups`; use the timestamped directory
printed by the backup command if it differs from this example.

Restore stops the control service, replaces the database and `/data`, and then
restarts control. Confirm the selected backup before using `--yes`; after
recovery, inspect workflows, Inbox/Outbox state, audit history, and GitHub for
uncertain external effects before resuming work.

Stop local services with:

```sh
scripts/down.sh
```

This stops the Compose stack. Preserve `secrets/`, PostgreSQL volumes, and
control data when you intend to resume later.
