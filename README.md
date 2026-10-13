# GitHub-Native Multi-Agent Workflow MVP

This repository contains the MVP control service for Issue-driven development
and independent PR review. GitHub Issues and pull requests are the collaboration
surface; a Go control process handles policy, durable state, scheduling,
isolated attempts, verification, and gated GitHub effects. Dev and Review use
separate role prompts and reports. The current design supports up to two
builders and two reviewers, with PostgreSQL persistence, webhook Inbox and
external-effect Outbox recovery, and a human Maintainer as the only merger.

The MVP can run a deterministic Fake walkthrough without model or GitHub
credentials. The pilot fixture intentionally begins with a failing `Add` test
for the Issue **Repair Add**; Fake mode demonstrates a fixed run through
`AWAITING_MAINTAINER`. A real Responses walkthrough with `deepseek-v4.1-flash` has also completed
Dev, two Reviewers, Skeptic and the current-head GitHub merge gate on
[pilot PR #6](https://github.com/ycychome/ai-workflow-pilot/pull/6). This is a
bounded functional smoke run; the larger fault matrix and model-effectiveness
benchmark remain unevaluated. See the exact [delivery scope](docs/mvp-delivery.md).

## Start locally

The current local deployment runs on macOS with Colima (`ai-workflow` profile):
PostgreSQL and the trusted control process run as Docker Compose services. The
host exposes only `127.0.0.1:8080` and `127.0.0.1:8081`. Linux can use a local
Docker Engine and Docker Compose directly. Run on a machine with Docker and
Docker Compose:

```sh
scripts/up.sh
```

Open the web dashboard at [http://127.0.0.1:8081](http://127.0.0.1:8081). The local
login token is stored in `secrets/dashboard-token`; `up.sh` keeps it in the
file instead of printing it into chat. Enter it in the dashboard. Open **Settings**
to save the Responses Base URL, Dev and Review model names, and API token. Saved
settings persist locally; the browser never reads a saved token back. The web
diagnostic skips a model call in Fake mode and while required endpoint,
token, Dev, or Review model fields are absent.

Port 8080 is reserved for `/healthz`, `/readyz`, and `POST /webhooks/github`;
the dashboard and GitHub App setup live on port 8081.

The control process runs as `control -config /secrets/config.yaml`. Its
configuration-aware diagnostic CLI is `control -doctor -config
/secrets/config.yaml`. To walk through the fixed pilot path without external
calls, select Fake mode and create the local demo.

## Workspace capacity

Each sandbox workspace has a filesystem capacity limit, including checked-out
source, Git metadata, generated files and build caches under `/workspace`.
`runner.workspace_bytes` defaults to `536870912` (512 MiB); configuration accepts
positive integer byte counts up to 8 GiB. Docker stores each workspace in a
bounded tmpfs-backed named volume, held mounted by a controller-owned keeper
container for that workspace's lifetime. Normal cleanup removes the keeper and
volume. Unsupported tmpfs mounting or quota setup fails workspace creation;
there is no fallback to an unbounded disk volume.

Tmpfs consumes Docker-host RAM and may use swap; on Colima this means the VM's
resources. Allow for all concurrent workspaces as well as runner memory limits.
Changing the capacity invalidates current verification and creates a new manager
for future workspaces. See the [runbook](docs/runbook.md#workspace-capacity-and-cleanup)
for configuration, restart and cleanup details.

## Connect GitHub

After signing in to the dashboard, use the GitHub App setup button or open
[http://127.0.0.1:8081/setup](http://127.0.0.1:8081/setup). The manifest and
callback flow creates and installs the App. Keep setup local. If GitHub needs
to reach a local machine, `scripts/tunnel.sh` exposes only the webhook proxy's
`POST /webhooks/github` route; it does not expose the dashboard or setup page.
The Cloudflare quick-tunnel URL is temporary and can change when the tunnel
process restarts; update the GitHub App webhook URL when it changes. For a
fixed-domain server deployment, set the public webhook URL to that domain and
update the App's webhook endpoint to `https://<domain>/webhooks/github`.

The service writes checks and pull-request updates through a durable Outbox,
reconciles webhook deliveries through an Inbox, and keeps PostgreSQL plus
`/data` state across restarts. Passing review can publish a check but cannot
merge a PR. A Maintainer reviews and merges through GitHub.

## Pilot fixture

[`fixtures/pilot`](fixtures/pilot) is a standalone standard-library Go module
for the first repository. Its initial `go test ./...` is expected to fail at
`TestAdd`; this is the issue fixture, not a passing baseline. Its README lists
ten bounded tasks with explicit acceptance criteria.

## Operations and contracts

- [MVP delivery and usage](docs/mvp-delivery.md): installed pilot, dashboard,
  operational controls, and the exact validation scope.
- [Local runbook](docs/runbook.md): model/GitHub setup, doctor, tunnel, backup,
  restore, and shutdown.
- [Workflow Contracts v1](contracts/README.md): domain records, lifecycle,
  verification identity, Runner/Gate sources, and event persistence.
- [Reviewed architecture](docs/plan/Architecture_v0.4_Reviewed.md): product
  scope, component model, design decisions, and pending acceptance requirements.

Back up both PostgreSQL and `/data` before recovery exercises. Restore is
destructive and requires `scripts/restore.sh --yes <backup-directory>`.
