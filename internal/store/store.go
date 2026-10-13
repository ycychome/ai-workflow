package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/domain"
	"time"
)

var ErrStale = errors.New("workflow generation, lease or version changed")
var ErrNoChange = errors.New("workflow update has no changes")

type Store struct{ DB *pgxpool.Pool }

type commandContextKey struct{}
type expectedWorkflowState struct {
	Generation int64
	HeadSHA    string
}
type expectedStateContextKey struct{}

// WithExpectedState binds a user-confirmed command to the workflow snapshot
// observed by the client. Update rechecks this while holding the workflow row lock.
func WithExpectedState(ctx context.Context, generation int64, headSHA string) context.Context {
	return context.WithValue(ctx, expectedStateContextKey{}, expectedWorkflowState{Generation: generation, HeadSHA: headSHA})
}

// WithCommand marks an Update as the transactional effect of a user command.
// The command key is stored alongside workflow changes and actions so retries
// of the same GitHub comment cannot execute the command twice.
func WithCommand(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, commandContextKey{}, key)
}

// WithoutCommand clears command deduplication for reconciliation work that
// must not consume the command's idempotency key before its own update.
func WithoutCommand(ctx context.Context) context.Context {
	return context.WithValue(ctx, commandContextKey{}, "")
}

func commandKey(ctx context.Context) string {
	key, _ := ctx.Value(commandContextKey{}).(string)
	return key
}

type Inbox struct {
	ID, Event string
	Payload   []byte
	Attempts  int
}
type Action struct {
	ID         string          `json:"id"`
	WorkflowID string          `json:"workflow_id"`
	Generation int64           `json:"generation"`
	Kind       string          `json:"kind"`
	Data       json.RawMessage `json:"data"`
	Attempts   int             `json:"attempts"`
}

func Open(ctx context.Context, url string) (*Store, error) {
	p, e := pgxpool.New(ctx, url)
	if e != nil {
		return nil, e
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	s := &Store{p}
	e = s.Migrate(ctx)
	return s, e
}
func (s *Store) Close() { s.DB.Close() }
func (s *Store) Migrate(ctx context.Context) error {
	_, e := s.DB.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS workflows(id text PRIMARY KEY, repository_id bigint NOT NULL, issue integer NOT NULL, phase text NOT NULL, control text NOT NULL, generation bigint NOT NULL, version bigint NOT NULL, data jsonb NOT NULL, lease_owner text NOT NULL DEFAULT '', lease_until timestamptz, updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(repository_id,issue));
CREATE INDEX IF NOT EXISTS workflows_schedule ON workflows(control,phase,lease_until);
CREATE TABLE IF NOT EXISTS workflow_metrics(workflow_id text PRIMARY KEY REFERENCES workflows(id), calls integer NOT NULL DEFAULT 0, input_tokens bigint NOT NULL DEFAULT 0, output_tokens bigint NOT NULL DEFAULT 0, runtime_seconds bigint NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS daily_usage(day date PRIMARY KEY, calls integer NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS inbox(id text PRIMARY KEY, event text NOT NULL,payload jsonb NOT NULL,state text NOT NULL DEFAULT 'received',attempts integer NOT NULL DEFAULT 0,lease_until timestamptz,next_attempt timestamptz NOT NULL DEFAULT now(),error text NOT NULL DEFAULT '',created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS outbox(id text PRIMARY KEY, workflow_id text NOT NULL REFERENCES workflows(id),generation bigint NOT NULL,kind text NOT NULL,data jsonb NOT NULL,state text NOT NULL DEFAULT 'pending',attempts integer NOT NULL DEFAULT 0,lease_until timestamptz,next_attempt timestamptz NOT NULL DEFAULT now(),receipt jsonb,error text NOT NULL DEFAULT '',created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS attempts(id text PRIMARY KEY,workflow_id text NOT NULL REFERENCES workflows(id),intent_id text NOT NULL,generation bigint NOT NULL,lease_epoch bigint NOT NULL,state text NOT NULL,report jsonb,evidence jsonb,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS audit(id bigserial PRIMARY KEY,workflow_id text,kind text NOT NULL,data jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS gate_decisions(id text PRIMARY KEY,workflow_id text NOT NULL REFERENCES workflows(id),generation bigint NOT NULL,verification_key text NOT NULL,data jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS processed_commands(command_id text NOT NULL,workflow_id text NOT NULL REFERENCES workflows(id),created_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(command_id,workflow_id));
INSERT INTO schema_migrations(version) VALUES(1) ON CONFLICT DO NOTHING;
`)
	return e
}
func (s *Store) Inbox(ctx context.Context, id, event string, payload []byte) error {
	if id == "" || !json.Valid(payload) {
		return errors.New("invalid delivery ID or payload")
	}
	_, e := s.DB.Exec(ctx, "INSERT INTO inbox(id,event,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", id, event, payload)
	return e
}
func (s *Store) ClaimInbox(ctx context.Context) (Inbox, error) {
	var x Inbox
	err := s.DB.QueryRow(ctx, `
WITH next AS (
  SELECT id FROM inbox
  WHERE state IN ('received','retry_wait','processing')
    AND next_attempt <= now()
    AND (lease_until IS NULL OR lease_until < now())
  ORDER BY created_at, id
  FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE inbox
SET state='processing', lease_until=now()+interval '90 seconds', attempts=attempts+1
FROM next WHERE inbox.id=next.id
RETURNING inbox.id,event,payload,attempts`).Scan(&x.ID, &x.Event, &x.Payload, &x.Attempts)
	return x, err
}

// FinishInbox is fenced by the claim attempt. A worker whose lease expired
// cannot overwrite the state written by a later claimant.
func (s *Store) FinishInbox(ctx context.Context, x Inbox, processErr error) error {
	state, message := "processed", ""
	if processErr != nil {
		state, message = "retry_wait", processErr.Error()
		if x.Attempts >= 8 {
			state = "dead_letter"
		}
	}
	tag, err := s.DB.Exec(ctx, `UPDATE inbox SET state=$2,error=$3,lease_until=NULL,
next_attempt=now()+$4*interval '1 second' WHERE id=$1 AND attempts=$5 AND state='processing'`,
		x.ID, state, message, min(300, 1<<min(x.Attempts, 8)), x.Attempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}
func (s *Store) Create(ctx context.Context, w domain.Workflow) error {
	_, err := s.CreateIfAbsent(ctx, w)
	return err
}

// CreateIfAbsent creates a workflow and its metrics row atomically. The bool
// reports whether this call won the repository/issue uniqueness race.
func (s *Store) CreateIfAbsent(ctx context.Context, w domain.Workflow) (bool, error) {
	w.Version = 1
	w.Generation = 1
	w.CreatedAt = time.Now().UTC()
	w.UpdatedAt = w.CreatedAt
	if w.Control == "" {
		w.Control = "ACTIVE"
	}
	if w.Phase == "" {
		w.Phase = "INTAKE"
	}
	b, _ := json.Marshal(w)
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, `INSERT INTO workflows(id,repository_id,issue,phase,control,generation,version,data) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, w.ID, w.RepositoryID, w.Issue, w.Phase, w.Control, w.Generation, w.Version, b)
	if e != nil {
		return false, e
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	_, e = tx.Exec(ctx, `INSERT INTO workflow_metrics(workflow_id) VALUES($1) ON CONFLICT DO NOTHING`, w.ID)
	if e != nil {
		return false, e
	}
	if e = tx.Commit(ctx); e != nil {
		return false, e
	}
	return true, nil
}

const workflowSelect = `SELECT w.data,w.version,w.generation,w.phase,w.control,w.lease_owner,COALESCE(w.lease_until,'epoch'::timestamptz),m.calls,m.input_tokens,m.output_tokens,m.runtime_seconds FROM workflows w JOIN workflow_metrics m ON m.workflow_id=w.id`

func scan(row pgx.Row) (domain.Workflow, error) {
	var w domain.Workflow
	var b []byte
	var phase, control, owner string
	var version, generation, in, out, runtime int64
	var calls int
	var until time.Time
	e := row.Scan(&b, &version, &generation, &phase, &control, &owner, &until, &calls, &in, &out, &runtime)
	if e != nil {
		return w, e
	}
	e = json.Unmarshal(b, &w)
	w.Version = version
	w.Generation = generation
	w.Phase = phase
	w.Control = control
	w.LeaseOwner = owner
	w.LeaseUntil = until
	w.ModelCalls = calls
	w.InputTokens = in
	w.OutputTokens = out
	w.RuntimeSeconds = runtime
	return w, e
}
func (s *Store) Get(ctx context.Context, id string) (domain.Workflow, error) {
	return scan(s.DB.QueryRow(ctx, workflowSelect+" WHERE w.id=$1", id))
}
func (s *Store) List(ctx context.Context) ([]domain.Workflow, error) {
	rows, e := s.DB.Query(ctx, workflowSelect+" ORDER BY w.updated_at DESC LIMIT 200")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.Workflow
	for rows.Next() {
		w, e := scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

const workflowSchedulerLock int64 = 0x6169776600000001

func (s *Store) Claim(ctx context.Context, owner string) (domain.Workflow, error) {
	return s.ClaimWithLimit(ctx, owner, 2)
}

// ClaimWithLimit claims one workflow while enforcing a global concurrency
// ceiling across control processes. Only valid leases on runnable automatic
// phases count; maintainer-waiting and other human-owned states never occupy a
// slot.
func (s *Store) ClaimWithLimit(ctx context.Context, owner string, maxActive int) (domain.Workflow, error) {
	if maxActive < 1 || maxActive > 4 {
		return domain.Workflow{}, errors.New("workflow concurrency must be between 1 and 4")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return domain.Workflow{}, err
	}
	defer tx.Rollback(ctx)

	// Serialize slot checks across all control processes. The row-level
	// candidate lock below and the per-workflow lease remain the work fence.
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, workflowSchedulerLock).Scan(&locked); err != nil {
		return domain.Workflow{}, err
	}
	if !locked {
		return domain.Workflow{}, pgx.ErrNoRows
	}
	var active int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM workflows
WHERE lease_owner<>'' AND lease_until>now() AND control='ACTIVE'
  AND phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')`).Scan(&active)
	if err != nil {
		return domain.Workflow{}, err
	}
	if active >= maxActive {
		return domain.Workflow{}, pgx.ErrNoRows
	}

	var id string
	var repositoryID int64
	err = tx.QueryRow(ctx, `SELECT w.id,w.repository_id
FROM workflows w
WHERE w.control='ACTIVE'
  AND w.phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')
  AND (w.lease_until IS NULL OR w.lease_until<now())
  AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.workflow_id=w.id AND o.kind IN('publish','gate','ready') AND o.state IN('pending','running','unknown','retry_wait'))
ORDER BY w.updated_at
FOR UPDATE OF w SKIP LOCKED
LIMIT 1`).Scan(&id, &repositoryID)
	if err != nil {
		return domain.Workflow{}, err
	}

	tag, err := tx.Exec(ctx, `UPDATE workflows SET lease_owner=$2,lease_until=now()+interval '90 seconds'
WHERE id=$1 AND repository_id=$3 AND control='ACTIVE'
  AND phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')
  AND (lease_until IS NULL OR lease_until<now())`, id, owner, repositoryID)
	if err != nil {
		return domain.Workflow{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.Workflow{}, pgx.ErrNoRows
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Workflow{}, err
	}
	return s.Get(ctx, id)
}
func (s *Store) Renew(ctx context.Context, w domain.Workflow) error {
	tag, e := s.DB.Exec(ctx, "UPDATE workflows SET lease_until=now()+interval '90 seconds' WHERE id=$1 AND lease_owner=$2 AND generation=$3 AND control='ACTIVE' AND lease_until>now()", w.ID, w.LeaseOwner, w.Generation)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}
func (s *Store) Release(ctx context.Context, w domain.Workflow) {
	_, _ = s.DB.Exec(ctx, "UPDATE workflows SET lease_owner='',lease_until=NULL WHERE id=$1 AND lease_owner=$2", w.ID, w.LeaseOwner)
}
func insertActions(ctx context.Context, tx pgx.Tx, actions []Action) error {
	for _, a := range actions {
		_, e := tx.Exec(ctx, "INSERT INTO outbox(id,workflow_id,generation,kind,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", a.ID, a.WorkflowID, a.Generation, a.Kind, a.Data)
		if e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) Save(ctx context.Context, w *domain.Workflow, actions ...Action) error {
	old := w.Version
	w.Version++
	w.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(w)
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		w.Version = old
		return e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, "UPDATE workflows SET data=$2,phase=$3,control=$4,version=$5,updated_at=now() WHERE id=$1 AND version=$6 AND generation=$7 AND lease_owner=$8 AND lease_until>now()", w.ID, b, w.Phase, w.Control, w.Version, old, w.Generation, w.LeaseOwner)
	if e != nil {
		w.Version = old
		return e
	}
	if tag.RowsAffected() != 1 {
		w.Version = old
		return ErrStale
	}
	if w.Review.Key != "" && (w.Review.Verdict == "PASS" || w.Review.Verdict == "CHANGES_REQUESTED" || w.Review.Verdict == "BLOCKED") {
		d := domain.GateDecision{ID: domain.Digest([]any{w.ID, w.Generation, w.Version, w.Review.Key, w.Review.Verdict}), WorkflowID: w.ID, Generation: w.Generation, VerificationKey: w.Review.Key, SpecDigest: w.Spec.Digest, BaseSHA: w.BaseSHA, HeadSHA: w.HeadSHA, Verdict: w.Review.Verdict, Reason: w.Review.Reason, Evidence: w.Evidence, Coverage: w.Review.Obligations, Findings: w.Review.Findings, CreatedAt: time.Now().UTC()}
		payload, _ := json.Marshal(d)
		if _, e = tx.Exec(ctx, "INSERT INTO gate_decisions(id,workflow_id,generation,verification_key,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", d.ID, w.ID, w.Generation, w.Review.Key, payload); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO audit(workflow_id,kind,data) VALUES($1,'gate_decision',$2)", w.ID, payload); e != nil {
			return e
		}
	}
	if e = insertActions(ctx, tx, actions); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Update is the serialized command/event entrypoint. Its callback never runs external effects.
func (s *Store) Update(ctx context.Context, id string, fn func(*domain.Workflow) ([]Action, error)) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	w, err := scan(tx.QueryRow(ctx, workflowSelect+" WHERE w.id=$1 FOR UPDATE OF w", id))
	if err != nil {
		return err
	}
	if expected, ok := ctx.Value(expectedStateContextKey{}).(expectedWorkflowState); ok && (w.Generation != expected.Generation || w.HeadSHA != expected.HeadSHA) {
		return ErrStale
	}
	oldPhase := w.Phase
	oldControl := w.Control
	oldReview, _ := json.Marshal(w.Review)
	oldForceApproval := domain.Digest(w.ForceApproval)
	if key := commandKey(ctx); key != "" {
		tag, err := tx.Exec(ctx, `INSERT INTO processed_commands(command_id,workflow_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
	}
	oldGeneration := w.Generation
	actions, err := fn(&w)
	if err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	if w.Generation < oldGeneration {
		return errors.New("workflow generation cannot move backwards")
	}
	w.Version++
	w.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE workflows SET data=$2,phase=$3,control=$4,generation=$5,version=$6,updated_at=now() WHERE id=$1", id, b, w.Phase, w.Control, w.Generation, w.Version)
	if err != nil {
		return err
	}
	if err = obsoleteUnstartedActions(ctx, tx, id, oldGeneration, oldControl, w.Generation, w.Control); err != nil {
		return err
	}
	if err = insertActions(ctx, tx, actions); err != nil {
		return err
	}
	if w.ForceApproval != nil && domain.Digest(w.ForceApproval) != oldForceApproval {
		kind := "force_accept_issue_development"
		if w.ForceApproval.Kind == "pr_merge_gate" {
			kind = "force_accept_pr_gate"
		}
		payload, marshalErr := json.Marshal(map[string]any{
			"actor": w.ForceApproval.Actor, "command": w.ForceApproval.Command,
			"approved_at": w.ForceApproval.ApprovedAt, "old_phase": oldPhase, "old_control": oldControl, "old_review": json.RawMessage(oldReview),
			"pr": w.PR, "prior_constraints": w.ForceApproval.PriorConstraints,
			"discussion_context": w.ForceApproval.Discussion,
			"generation":         w.Generation, "spec_digest": w.ForceApproval.SpecDigest,
			"policy_digest": w.ForceApproval.PolicyDigest, "verification_key": w.ForceApproval.VerificationKey,
			"issue_hash": w.ForceApproval.IssueHash, "base_sha": w.ForceApproval.BaseSHA,
			"head_sha": w.ForceApproval.HeadSHA, "bypassed": w.ForceApproval.Bypassed,
		})
		if marshalErr != nil {
			return marshalErr
		}
		if _, err = tx.Exec(ctx, "INSERT INTO audit(workflow_id,kind,data) VALUES($1,$2,$3)", id, kind, payload); err != nil {
			return err
		}
		if w.ForceApproval.Kind == "pr_merge_gate" {
			decision := domain.GateDecision{ID: domain.Digest([]any{w.ID, w.Generation, w.ForceApproval.VerificationKey, "FORCED"}), WorkflowID: w.ID, Generation: w.Generation, VerificationKey: w.ForceApproval.VerificationKey, SpecDigest: w.Spec.Digest, BaseSHA: w.BaseSHA, HeadSHA: w.HeadSHA, Verdict: "FORCED", Reason: "explicit maintainer override; automated review/verification was bypassed", Evidence: w.Evidence, Coverage: w.Review.Obligations, Findings: w.Review.Findings, CreatedAt: w.ForceApproval.ApprovedAt}
			decisionPayload, decisionErr := json.Marshal(decision)
			if decisionErr != nil {
				return decisionErr
			}
			if _, err = tx.Exec(ctx, "INSERT INTO gate_decisions(id,workflow_id,generation,verification_key,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", decision.ID, w.ID, w.Generation, decision.VerificationKey, decisionPayload); err != nil {
				return err
			}
		}
	}
	if err = obsoleteUnstartedActions(ctx, tx, id, oldGeneration, oldControl, w.Generation, w.Control); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func controlStopsPublication(control string) bool {
	switch control {
	case "PAUSING", "PAUSED", "CANCELLING", "CANCELLED", "TAKEOVER":
		return true
	default:
		return false
	}
}

// obsoleteUnstartedActions retires local, not-yet-sent publish, ready and passing-gate
// effects in the same transaction that advances the generation or starts a
// pause/cancel/takeover. Running and unknown effects remain for reconciliation.
func obsoleteUnstartedActions(ctx context.Context, tx pgx.Tx, workflowID string, oldGeneration int64, oldControl string, generation int64, control string) error {
	generationChanged := generation != oldGeneration
	enteredStop := !controlStopsPublication(oldControl) && controlStopsPublication(control)
	if !generationChanged && !enteredStop && !controlStopsPublication(control) {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id,generation,kind,data FROM outbox
WHERE workflow_id=$1 AND kind IN ('publish','gate','ready')
AND state IN ('pending','retry_wait') ORDER BY created_at,id`, workflowID)
	if err != nil {
		return err
	}
	type candidate struct {
		id         string
		generation int64
		kind       string
		data       []byte
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.generation, &c.kind, &c.data); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, c := range candidates {
		old := c.generation < generation
		stopping := controlStopsPublication(control)
		if !(old || stopping) {
			continue
		}
		if c.kind == "gate" && !actionIsPassingGate(c.data) {
			continue
		}
		_, err := tx.Exec(ctx, `UPDATE outbox SET state='obsolete',error='workflow generation or control changed',lease_until=NULL
WHERE id=$1 AND generation=$2 AND state IN ('pending','retry_wait')`, c.id, c.generation)
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ReserveRequest(ctx context.Context, id string, generation int64, issueLimit, dailyLimit int) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var gen int64
	var mode string
	e = tx.QueryRow(ctx, "SELECT generation,control FROM workflows WHERE id=$1 FOR SHARE", id).Scan(&gen, &mode)
	if e != nil {
		return e
	}
	if gen != generation || mode != "ACTIVE" {
		return ErrStale
	}
	var n int
	e = tx.QueryRow(ctx, "SELECT calls FROM workflow_metrics WHERE workflow_id=$1 FOR UPDATE", id).Scan(&n)
	if e != nil {
		return e
	}
	if n >= issueLimit {
		return errors.New("issue model request budget exhausted")
	}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	_, e = tx.Exec(ctx, "INSERT INTO daily_usage(day) VALUES($1) ON CONFLICT DO NOTHING", day)
	if e != nil {
		return e
	}
	var daily int
	e = tx.QueryRow(ctx, "SELECT calls FROM daily_usage WHERE day=$1 FOR UPDATE", day).Scan(&daily)
	if e != nil {
		return e
	}
	if daily >= dailyLimit {
		return errors.New("daily model request budget exhausted")
	}
	_, e = tx.Exec(ctx, "UPDATE daily_usage SET calls=calls+1 WHERE day=$1", day)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "UPDATE workflow_metrics SET calls=calls+1 WHERE workflow_id=$1", id)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) Usage(ctx context.Context, id string, in, out int64) error {
	_, e := s.DB.Exec(ctx, "UPDATE workflow_metrics SET input_tokens=input_tokens+$2,output_tokens=output_tokens+$3 WHERE workflow_id=$1", id, in, out)
	return e
}
func (s *Store) Runtime(ctx context.Context, id string, seconds int64) error {
	_, e := s.DB.Exec(ctx, "UPDATE workflow_metrics SET runtime_seconds=runtime_seconds+$2 WHERE workflow_id=$1", id, seconds)
	return e
}
func (s *Store) Attempt(ctx context.Context, id, wid, intent string, gen, epoch int64, state string, report, evidence any) error {
	return s.AttemptOwned(ctx, id, wid, intent, gen, epoch, "", state, report, evidence)
}

// AttemptOwned records an attempt only while the workflow still has the
// expected live lease. Supplying owner additionally fences writers belonging
// to a prior worker process; the original Attempt method remains compatible.
func (s *Store) AttemptOwned(ctx context.Context, id, wid, intent string, gen, epoch int64, owner, state string, report, evidence any) error {
	r, err := json.Marshal(report)
	if err != nil {
		return err
	}
	ev, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	var ownerClause string
	args := []any{id, wid, intent, gen, epoch, state, r, ev}
	if owner != "" {
		ownerClause = " AND lease_owner=$9"
		args = append(args, owner)
	}
	query := `INSERT INTO attempts(id,workflow_id,intent_id,generation,lease_epoch,state,report,evidence)
SELECT $1,$2,$3,$4,$5,$6,$7,$8
WHERE EXISTS (
  SELECT 1 FROM workflows WHERE id=$2 AND generation=$4 AND control='ACTIVE'
    AND lease_owner<>'' AND lease_until>now()` + ownerClause + `
) AND NOT EXISTS (
  SELECT 1 FROM attempts WHERE workflow_id=$2 AND intent_id=$3 AND generation=$4 AND lease_epoch>$5
)
ON CONFLICT(id) DO UPDATE SET state=EXCLUDED.state,report=EXCLUDED.report,
evidence=EXCLUDED.evidence,updated_at=now()
WHERE attempts.generation=EXCLUDED.generation AND attempts.lease_epoch<=EXCLUDED.lease_epoch`
	tag, err := s.DB.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}

func (s *Store) Audit(ctx context.Context, id, kind string, data any) {
	b, _ := json.Marshal(data)
	_, _ = s.DB.Exec(ctx, "INSERT INTO audit(workflow_id,kind,data) VALUES($1,$2,$3)", id, kind, b)
}

// AuditList returns newest-first audit entries in the shape consumed by the
// local dashboard, including the original structured data and timestamp.
func (s *Store) AuditList(ctx context.Context, workflowID string) ([]map[string]any, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,workflow_id,kind,data,created_at FROM audit
WHERE workflow_id=$1 ORDER BY id DESC LIMIT 500`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var wid, kind string
		var raw []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &wid, &kind, &raw, &createdAt); err != nil {
			return nil, err
		}
		var data any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		entries = append(entries, map[string]any{
			"id": id, "workflow_id": wid, "kind": kind, "data": data, "created_at": createdAt,
		})
	}
	return entries, rows.Err()
}

func (s *Store) ClaimAction(ctx context.Context) (Action, error) {
	var a Action
	err := s.DB.QueryRow(ctx, `WITH next AS (
  SELECT o.id FROM outbox o
  WHERE o.state IN ('pending','retry_wait','unknown','running')
    AND o.next_attempt<=now() AND (o.lease_until IS NULL OR o.lease_until<now())
    AND NOT EXISTS (
      SELECT 1 FROM outbox earlier
      WHERE earlier.workflow_id=o.workflow_id
        AND (earlier.created_at<o.created_at OR (earlier.created_at=o.created_at AND earlier.id<o.id))
        AND earlier.state IN ('pending','retry_wait','unknown','running')
    )
  ORDER BY o.created_at,o.id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE outbox SET state='running',lease_until=now()+interval '90 seconds',attempts=attempts+1
FROM next WHERE outbox.id=next.id
RETURNING outbox.id,workflow_id,generation,kind,data,attempts`).Scan(&a.ID, &a.WorkflowID, &a.Generation, &a.Kind, &a.Data, &a.Attempts)
	return a, err
}

func actionIsPassingGate(data []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	for _, key := range []string{"pass", "passed", "success", "approved"} {
		var value bool
		if json.Unmarshal(fields[key], &value) == nil && value {
			return true
		}
	}
	for _, key := range []string{"conclusion", "verdict", "status"} {
		var value string
		if json.Unmarshal(fields[key], &value) == nil && (value == "success" || value == "PASS" || value == "passed") {
			return true
		}
	}
	return false
}

// StartAction is the side-effect authorization point. It locks the workflow
// row used by Update, so a pause/cancel and a passing publish cannot both be
// granted as if they happened first. Call it after ClaimAction and immediately
// before starting the external request.
func (s *Store) StartAction(ctx context.Context, a Action) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var generation int64
	var control, phase string
	err = tx.QueryRow(ctx, "SELECT generation,control,phase FROM workflows WHERE id=$1 FOR UPDATE", a.WorkflowID).Scan(&generation, &control, &phase)
	if err != nil {
		return err
	}
	var attempts int
	var state string
	err = tx.QueryRow(ctx, "SELECT attempts,state FROM outbox WHERE id=$1 FOR UPDATE", a.ID).Scan(&attempts, &state)
	if err != nil {
		return err
	}
	if attempts != a.Attempts || state != "running" {
		return ErrStale
	}
	requiresCurrentActive := a.Kind == "publish" || a.Kind == "ready" || (a.Kind == "gate" && actionIsPassingGate(a.Data))
	if requiresCurrentActive && (generation != a.Generation || control != "ACTIVE") {
		return ErrStale
	}
	if a.Kind == "gate" && actionIsPassingGate(a.Data) && phase != "READY" && phase != "AWAITING_MAINTAINER" {
		return ErrStale
	}
	return tx.Commit(ctx)
}

// AbandonAction retires a claimed action that StartAction rejected. It is
// fenced by the claim attempt, preserving any later claimant's state.
func (s *Store) AbandonAction(ctx context.Context, a Action, reason string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE outbox SET state='obsolete',error=$3,lease_until=NULL
WHERE id=$1 AND attempts=$2 AND state='running'`, a.ID, a.Attempts, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}

func (s *Store) FinishAction(ctx context.Context, a Action, receipt any, effectErr error, unknown bool) error {
	state, message := "done", ""
	if effectErr != nil {
		state, message = "retry_wait", effectErr.Error()
		if unknown {
			state = "unknown"
		} else if a.Attempts >= 8 {
			state = "dead_letter"
		}
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `UPDATE outbox SET state=$3,error=$4,receipt=$5,lease_until=NULL,
next_attempt=now()+$6*interval '1 second'
WHERE id=$1 AND attempts=$2 AND state='running'`, a.ID, a.Attempts, state, message, b, min(300, 1<<min(a.Attempts, 8)))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}

func (s *Store) PendingWrites(ctx context.Context, id string) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1
AND state IN ('running','unknown') AND kind IN ('publish','gate','ready')`, id).Scan(&n)
	return n, err
}
func NewAction(w domain.Workflow, kind string, data any) Action {
	b, _ := json.Marshal(data)
	return Action{ID: fmt.Sprintf("%s:%d:%d:%s:%s", w.ID, w.Generation, w.Version, kind, domain.Digest(data)[:12]), WorkflowID: w.ID, Generation: w.Generation, Kind: kind, Data: b}
}
