# Local MVP Runbook

This guide runs the control service on one trusted local machine. The web
dashboard and setup page bind to loopback on port 8081. Port 8080 serves only
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
diagnostic mode is `control -doctor -config /secrets/config.yaml`; it checks
configured dependencies. The web **Run diagnostics** action uses the same
configuration-aware checks. When the Responses endpoint, token, or both role
models are unconfigured, doctor reports that state and skips the model request.
The diagnostic only contacts the configured model service after the required
fields are present.

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

Use the **GitHub App registration** button, or open
`http://127.0.0.1:8081/setup`. The manifest flow creates the App and returns to
the local callback. Complete installation and repository authorization, then
save the App ID, installation ID, private key, webhook secret, and selected
repository in local configuration. Do not expose port 8081 publicly.

When GitHub must reach a machine behind NAT, start the webhook tunnel in a
separate terminal:

```sh
scripts/tunnel.sh
```

The tunnel targets the local allowlist proxy. It forwards only `POST
/webhooks/github` to the control service. Dashboard, health, settings, and App
setup endpoints are not tunneled. Put the printed HTTPS URL plus
`/webhooks/github` into the GitHub App webhook configuration. Verify the
delivery signature and a real test delivery before treating integration as
ready.

## Control and merge

Use the dashboard to inspect workflow phase, control state, event audit, task
reports, evidence, model usage, and review coverage. Pause or take over a
workflow when review evidence is incomplete or uncertain. The service uses a
durable PostgreSQL database and `/data` volume, with a webhook Inbox and an
external-effect Outbox for restart recovery and reconciliation. Inbox/Outbox
processing is at least once; uncertain GitHub writes require reconciliation.

MVP worker settings allow up to two builders and two reviewers. Work remains
bound to a generation, lease, path scope, and verification identity. A passing
gate publishes a check for the current candidate but never merges it. A
Maintainer must inspect the pull request and merge through GitHub's normal
interface.

## Backup, restore, and shutdown

Create a database and `/data` archive with:

```sh
scripts/backup.sh [backup-root]
```

The script writes `postgres.dump` and `data.tar.gz` under a timestamped
directory. Copy that directory to protected storage and record its timestamp.
Restore requires the explicit destructive acknowledgement:

```sh
scripts/restore.sh --yes backups/20261012T120000Z
```

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
