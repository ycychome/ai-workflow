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
`AWAITING_MAINTAINER`. Real-model evaluation is a separate acceptance step:
configure a Responses-compatible endpoint, token, Dev model, and Review model,
then run the required evaluation. No successful real-model evaluation is
claimed until that has happened.

## Start locally

Run on a machine with Docker and Docker Compose:

```sh
scripts/up.sh
```

Open the web dashboard at [http://127.0.0.1:8081](http://127.0.0.1:8081). The local
login token is stored in `secrets/dashboard-token`; `up.sh` keeps it in the
file instead of printing it into chat. Enter it in the dashboard. Open **Settings**
to save the Responses Base URL, Dev and Review model names, and API token. Saved
settings persist locally; the browser never reads a saved token back. Doctor
checks the real endpoint only after the service is configured, and skips any
model call while required endpoint/token/model fields are absent.

Port 8080 is reserved for `/healthz`, `/readyz`, and `POST /webhooks/github`;
the dashboard and GitHub App setup live on port 8081.

The control process runs as `control -config /secrets/config.yaml`. Its
configuration-aware diagnostic CLI is `control -doctor -config
/secrets/config.yaml`. To walk through the fixed pilot path without external
calls, select Fake mode and create the local demo.

## Connect GitHub

Use the GitHub App setup button or open
[http://127.0.0.1:8081/setup](http://127.0.0.1:8081/setup). The manifest and
callback flow creates and installs the App. Keep setup local. If GitHub needs
to reach a local machine, `scripts/tunnel.sh` exposes only the webhook proxy's
`POST /webhooks/github` route; it does not expose the dashboard or setup page.

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

- [Local runbook](docs/runbook.md): model/GitHub setup, doctor, tunnel, backup,
  restore, and shutdown.
- [Workflow Contracts v1](contracts/README.md): domain records, lifecycle,
  verification identity, Runner/Gate sources, and event persistence.
- [Reviewed architecture](docs/plan/Architecture_v0.4_Reviewed.md): product
  scope, component model, design decisions, and pending acceptance requirements.

Back up both PostgreSQL and `/data` before recovery exercises. Restore is
destructive and requires `scripts/restore.sh --yes <backup-directory>`.
