# Workflow Contracts v1

This document describes the MVP's versioned domain contract. The Go source of
truth is [`internal/domain/types.go`](../internal/domain/types.go). The store
persists workflow snapshots in PostgreSQL and keeps webhook Inbox, external
effect Outbox, attempts, metrics, and audit records in separate tables;
`internal/store/store.go` implements those persistence boundaries.

## Version and compatibility

Contract version `v1` covers the JSON fields and lifecycle vocabulary below.
Fields tagged with `omitempty` may be absent; other fields may be present with
their Go zero value. Consumers must ignore unknown JSON fields. Additive fields
are compatible. Renaming/removing a field, changing its JSON type, or changing
the meaning of a persisted status requires a new contract version and a data
migration. The current PostgreSQL schema records migration version `1`.

## Domain records

| Record | Purpose and important fields |
|---|---|
| `Workflow` | Durable Issue workflow identity and state: repository and Issue IDs, `phase`, `control`, `generation`, optimistic `version`, accepted `Spec`, Dev `Tasks`, PR/base/head SHAs, `Review`, `Evidence`, model usage, and timestamps. |
| `Spec` | Accepted requirement snapshot: `revision`, `digest`, title/description, testable `criteria`, non-goals, risk, source hash, and approval identity/state. `SpecDigest` excludes the digest field itself. |
| `Task` | Bounded work unit with stable ID, description, kind (`code`, `verification`, or `analysis`), allowed paths, dependencies, status, attempt count, lease epoch, integrated SHA, optional report, and review obligation IDs. |
| `Review` | One review snapshot: verification `key`, review tasks, complete obligations ledger, findings, skeptic and verification completion flags, verdict, and reason. |
| `Obligation` | A mandatory check tied to an acceptance criterion. An obligation is only complete when its result is `satisfied` and it carries evidence references. |
| `Finding` | A review claim with severity, category, source location, preconditions, expected/actual behavior, evidence type and references, state, verification key, and history. |
| `Evidence` | Verifiable artifact metadata, such as command, exit code, path/hash, creation time, base/head SHAs, and verification profile digest. |

JSON uses the struct field tags in `types.go`; examples below show the shape,
not a required complete object:

```json
{
  "id": "123456-42",
  "repository": "owner/repo",
  "issue": 42,
  "phase": "REVIEWING",
  "control": "ACTIVE",
  "generation": 3,
  "version": 18,
  "spec": {
    "revision": 1,
    "digest": "sha256-hex",
    "title": "Repair addition",
    "criteria": [{"id": "AC-1", "description": "Add returns the sum"}],
    "approved": true
  },
  "review": {
    "key": "verification-key",
    "obligations": [{"id": "OB-1", "criterion_id": "AC-1", "result": "satisfied", "evidence": ["EV-1"]}],
    "skeptic_complete": true,
    "verification_passed": true,
    "verdict": "PASS"
  }
}
```

## Lifecycle values

`phase` is business progress. The MVP uses `INTAKE`, `TRIAGE`, `PLANNING`,
`DEVELOPING`, `VERIFYING`, `REVIEWING`, `CHANGES_REQUESTED`, `READY`,
`SPEC_READY`, `NEEDS_INFO`, `REJECT_PROPOSED`, `REJECTED`, `DEFERRED`,
`AWAITING_MAINTAINER`, `WAITING_CONFIGURATION`, `BLOCKED`, and `MERGED`. Only active work
phases are schedulable; `domain.Runnable` is the code-level check.

`control` independently governs execution: `ACTIVE`, `PAUSING`, `PAUSED`,
`CANCELLING`, `CANCELLED`, `HUMAN_OWNED`, or `COMPLETED`. A non-active control state prevents scheduling even if the phase
would otherwise be runnable. Status strings remain open for additive values;
consumers must handle unknown values safely.

Task status values include `INTEGRATED` for validated Dev artifacts and `ACCEPTED`
for completed Review work. Obligation results include `unverified`, `satisfied`,
and `failed`. Finding severity and state values are defined by the producers;
the merge gate treats unresolved `critical`, `high`, and `medium` findings as
blocking. Evidence kinds distinguish model claims, source analysis, and
tool-verified results.

Dev code dependencies consume a committed `integrated_sha`. Analysis and
verification tasks consume independently recorded `NON_CODE_ARTIFACT` reports
with evidence from their own attempt. Empty legacy task kinds retain code
semantics. Analysis tasks have read tools; verification tasks can run commands
but cannot write through file tools. The supervisor rejects any source changes
from either non-code kind. Verification requires `result=satisfied` plus a
successful command transcript. These reports do not replace the independent
Review or trusted Verification Runner.

The dashboard exposes `gate_status`, `gate_conclusion`, `check_id`, `check_sha`
and `gate_confirmed_at` from GitHub receipts/reconciliation. A local review
verdict alone does not confirm that the remote check succeeded. Consumers must
compare the check SHA with the current head SHA before displaying its result.

## Identity and merge gate

`domain.VerificationKey` binds repository/PR identity, accepted spec digest,
generation, base and head SHAs, policy/profile versions, and runner image. A
review for a different key is obsolete. `domain.Gate` requires active control,
an approved spec, the current key, completed verification and skeptic review,
a non-empty fully evidenced obligations ledger covering every acceptance
criterion, and no unresolved blocking finding.

A blocked workflow can resume its unfinished Skeptic checkpoint only when
its current verification identity matches exactly, trusted verification passed,
and every Reviewer task is accepted. Resume advances generation and the
verification key, preserves the accepted proofs, and audits the key transition;
it never carries forward a completed Skeptic or PASS. Snapshot or policy changes
continue to require a full review.

The gate only authorizes publication of a successful check for the verified
candidate. It does not merge a pull request. A Maintainer reviews and merges
through GitHub's normal interface; a later base/head/spec/profile change
requires a new review key and gate decision.

## Runner, gate, and external event sources

The persisted audit stream uses `audit.kind` plus JSON `audit.data`; the Inbox
and Outbox retain their own delivery/effect state. Event kinds are source
labels, not a closed Go enum in v1.

| Source | Trigger / source record | State change or durable effect |
|---|---|---|
| GitHub webhook | Signed request headers and the normalized `github.WebhookEvent` projection (`action`, Issue/PR/comment, repository, installation, sender, refs, check run/suite). The delivery ID keys Inbox de-duplication. | Inbox is committed before asynchronous processing. A supported Issue event may create/update a Workflow; other events reconcile PR/review/check state. |
| Maintainer command | Authenticated web command such as pause, resume, cancel, replan, takeover, accept, reject, or defer. | Serialized `store.Update` changes workflow state and records audit data. Publish/gate work is queued as an Outbox `Action`. |
| Runner claim | `store.Claim` obtains an expiring lease for an active runnable workflow; worker attempt records include generation and lease epoch. | Runner result is accepted only against current generation/lease/version. Reports and tool evidence update tasks, review obligations, findings, usage, and audit data. |
| Gate evaluation | `domain.Gate(workflow, currentVerificationKey)` after verification and skeptic completion. | A passing decision queues a GitHub Check update through the Outbox; it does not merge. An obsolete key or missing evidence remains blocked/uncertain. |
| Outbox delivery | `store.ClaimAction` leases the next ordered side effect (`publish` or `gate`). | The GitHub effect and receipt are recorded with retry/unknown/dead-letter state. Retries are at-least-once and must reconcile uncertain writes; this is not end-to-end exactly-once delivery. |

The contract does not grant model reports authority to change permissions,
budgets, accepted criteria, or merge state. External state changes are decided
by the control process and stored events.
