// Package persistence provides a PostgreSQL inbox, aggregate CAS and outbox.
package persistence

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	c "github.com/ycychome/ai-workflow/internal/contracts"
)

//go:embed schema.sql
var schema embed.FS

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return nil, e
	}
	s := &Store{p}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	b, _ := schema.ReadFile("schema.sql")
	_, e = p.Exec(ctx, string(b))
	if e != nil {
		p.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() { s.Pool.Close() }
func (s *Store) RecordEvidence(ctx context.Context, w c.Workflow, digest, path string) error {
	_, e := s.Pool.Exec(ctx, `INSERT INTO run_evidence(digest,workflow_id,attempt_id,generation,epoch,path) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, digest, w.ID, w.Attempt.ID, w.Generation, w.Attempt.Epoch, path)
	return e
}
func (s *Store) Receive(ctx context.Context, id, event string, payload []byte) error {
	_, e := s.Pool.Exec(ctx, `INSERT INTO inbox(delivery_id,event,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, event, payload)
	return e
}

type Event struct {
	ID, Kind string
	Payload  []byte
	Epoch    int64
}

func (s *Store) ClaimEvent(ctx context.Context) (Event, error) {
	var ev Event
	e := s.Pool.QueryRow(ctx, `UPDATE inbox SET status='processing',epoch=epoch+1,attempts=attempts+1,lease_until=now()+interval '30 seconds'
 WHERE delivery_id=(SELECT delivery_id FROM inbox WHERE (status IN ('received','retry_wait') AND available_at<=now()) OR (status='processing' AND lease_until<now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING delivery_id,event,payload,epoch`).Scan(&ev.ID, &ev.Kind, &ev.Payload, &ev.Epoch)
	return ev, e
}
func (s *Store) FinishEvent(ctx context.Context, ev Event, err error) error {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	_, e := s.Pool.Exec(ctx, `UPDATE inbox SET status=CASE WHEN $3='' THEN 'processed' WHEN attempts>=8 THEN 'dead_letter' ELSE 'retry_wait' END,last_error=$3,available_at=now()+interval '2 seconds' WHERE delivery_id=$1 AND epoch=$2 AND status='processing'`, ev.ID, ev.Epoch, reason)
	return e
}
func (s *Store) Create(ctx context.Context, w c.Workflow) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// The repository lock prevents two separately delivered issues being admitted concurrently.
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, w.RepositoryID); e != nil {
		return e
	}
	var exists bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflows WHERE id=$1)`, w.ID).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return tx.Commit(ctx)
	}
	var busy bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflows WHERE repository_id=$1 AND data->>'control' NOT IN ('CANCELLED','HUMAN_OWNED') AND data->>'phase' NOT IN ('MERGED','AWAITING_MAINTAINER'))`, w.RepositoryID).Scan(&busy); e != nil {
		return e
	}
	if busy {
		return errors.New("repository has an active workflow")
	}
	b, e := json.Marshal(w)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO workflows(id,repository_id,data) VALUES($1,$2,$3)`, w.ID, w.RepositoryID, b)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) Get(ctx context.Context, id string) (c.Workflow, error) {
	var b []byte
	var w c.Workflow
	e := s.Pool.QueryRow(ctx, `SELECT data FROM workflows WHERE id=$1`, id).Scan(&b)
	if e != nil {
		return w, e
	}
	e = json.Unmarshal(b, &w)
	return w, e
}

// Update locks the workflow and atomically stores state and durable effects.
func (s *Store) Update(ctx context.Context, id string, fn func(*c.Workflow) ([]c.Action, error)) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var b []byte
	var w c.Workflow
	if e = tx.QueryRow(ctx, `SELECT data FROM workflows WHERE id=$1 FOR UPDATE`, id).Scan(&b); e != nil {
		return e
	}
	if e = json.Unmarshal(b, &w); e != nil {
		return e
	}
	version := w.Version
	actions, e := fn(&w)
	if e != nil {
		return e
	}
	w.Version++
	b, e = json.Marshal(w)
	if e != nil {
		return e
	}
	tag, e := tx.Exec(ctx, `UPDATE workflows SET data=$2,updated_at=now() WHERE id=$1 AND (data->>'version')::bigint=$3`, id, b, version)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return c.ErrStale
	}
	for _, a := range actions {
		b, e = json.Marshal(a)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO outbox(action_id,workflow_id,generation,epoch,kind,data) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, a.ID, id, a.Generation, a.Epoch, a.Kind, b)
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func Action(w c.Workflow, kind, suffix string, payload any) c.Action {
	b, _ := json.Marshal(payload)
	return c.Action{ID: fmt.Sprintf("%s:g%d:e%d:%s:%s", w.ID, w.Generation, w.Attempt.Epoch, kind, suffix), WorkflowID: w.ID, Generation: w.Generation, Epoch: w.Attempt.Epoch, Kind: kind, Payload: b, Status: "pending"}
}
func (s *Store) ListWork(ctx context.Context) ([]string, error) {
	rows, e := s.Pool.Query(ctx, `SELECT id FROM workflows WHERE data->>'control'='ACTIVE' AND data->>'phase' IN ('INTAKE','DEVELOPING','REVIEWING') ORDER BY updated_at`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) ClaimWork(ctx context.Context, id string, now time.Time, limit int) (c.Workflow, error) {
	var out c.Workflow
	e := s.Update(ctx, id, func(w *c.Workflow) ([]c.Action, error) {
		if w.Control != c.Active || (w.Phase != c.Intake && w.Phase != c.Developing && w.Phase != c.Reviewing) {
			return nil, c.ErrStale
		}
		if w.Attempt.Status == "OUTPUT_READY" {
			return nil, c.ErrStale
		}
		if w.Attempt.Status == "RUNNING" && now.Before(w.Attempt.ExpiresAt) {
			return nil, c.ErrStale
		}
		if w.Runs >= limit {
			w.Phase = c.Blocked
			w.Reason = "global attempt budget exhausted"
			return []c.Action{Action(*w, "gate", "budget", GatePayload{Verdict: c.Fail, Key: w.Key, Reason: w.Reason})}, nil
		}
		w.Runs++
		w.Attempt = c.Attempt{ID: fmt.Sprintf("%s-g%d-e%d", w.ID, w.Generation, w.Attempt.Epoch+1), Generation: w.Generation, Epoch: w.Attempt.Epoch + 1, ExpiresAt: now.Add(2 * time.Minute), Status: "RUNNING"}
		if w.Phase == c.Intake {
			w.Phase = c.Developing
		}
		out = *w
		return nil, nil
	})
	if e == nil && out.ID == "" {
		return out, c.ErrStale
	}
	return out, e
}

type GatePayload struct {
	Verdict  c.Verdict         `json:"verdict"`
	Key      c.VerificationKey `json:"key"`
	Reason   string            `json:"reason"`
	Evidence *c.Evidence       `json:"evidence,omitempty"`
}

func (s *Store) ClaimAction(ctx context.Context) (c.Action, error) {
	var b []byte
	var a c.Action
	var dispatch int64
	e := s.Pool.QueryRow(ctx, `UPDATE outbox SET status='sending',lease_until=now()+interval '5 minutes',attempts=attempts+1 WHERE action_id=(SELECT action_id FROM outbox WHERE (status IN ('pending','unknown') AND available_at<=now()) OR (status='sending' AND lease_until<now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING data,attempts`).Scan(&b, &dispatch)
	if e != nil {
		return a, e
	}
	e = json.Unmarshal(b, &a)
	a.DispatchEpoch = dispatch
	return a, e
}
func (s *Store) FinishAction(ctx context.Context, a c.Action, receipt any, err error) error {
	if err != nil {
		status := "unknown"
		if errors.Is(err, c.ErrStale) {
			status = "obsolete"
		}
		_, e := s.Pool.Exec(ctx, `UPDATE outbox SET status=$2,available_at=now()+interval '2 seconds',last_error=$3 WHERE action_id=$1 AND attempts=$4`, a.ID, status, err.Error(), a.DispatchEpoch)
		return e
	}
	b, e := json.Marshal(receipt)
	if e != nil {
		return e
	}
	_, e = s.Pool.Exec(ctx, `UPDATE outbox SET status='sent',receipt=$2 WHERE action_id=$1 AND attempts=$3`, a.ID, b, a.DispatchEpoch)
	return e
}
func (s *Store) RecordCandidateHead(ctx context.Context, a c.Action, head string) error {
	tag, e := s.Pool.Exec(ctx, `UPDATE outbox SET data=jsonb_set(data,'{payload,expected_head}',to_jsonb($3::text)) WHERE action_id=$1 AND attempts=$2 AND status='sending' AND lease_until>now()`, a.ID, a.DispatchEpoch, head)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return c.ErrStale
	}
	return nil
}

// Guard is called immediately before each remote write, including retries.
func (s *Store) Guard(ctx context.Context, a c.Action) error {
	var leased bool
	if e := s.Pool.QueryRow(ctx, `SELECT status='sending' AND attempts=$2 AND lease_until>now() FROM outbox WHERE action_id=$1`, a.ID, a.DispatchEpoch).Scan(&leased); e != nil {
		return e
	}
	if !leased {
		return c.ErrStale
	}
	w, e := s.Get(ctx, a.WorkflowID)
	if e != nil {
		return e
	}
	// Blocking projections are allowed during pause; code and PASS are fenced.
	if a.Kind == "gate" {
		var p GatePayload
		if e = json.Unmarshal(a.Payload, &p); e != nil {
			return e
		}
		if p.Verdict != c.Pass {
			if w.Generation != a.Generation {
				return c.ErrStale
			}
			if p.Verdict == c.Pending && w.Phase != c.Reviewing {
				return c.ErrStale
			}
			if w.Control == c.Active && w.Phase == c.AwaitingMaintainer {
				return c.ErrStale
			}
			return nil
		}
	}
	if w.Control != c.Active || w.Generation != a.Generation || w.Attempt.Epoch != a.Epoch {
		return c.ErrStale
	}
	return nil
}
func (s *Store) All(ctx context.Context) ([]c.Workflow, error) {
	rows, e := s.Pool.Query(ctx, `SELECT data FROM workflows ORDER BY id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var all []c.Workflow
	for rows.Next() {
		var b []byte
		var w c.Workflow
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &w); e != nil {
			return nil, e
		}
		all = append(all, w)
	}
	return all, rows.Err()
}
func (s *Store) RecoverExecutions(ctx context.Context) error {
	all, e := s.All(ctx)
	if e != nil {
		return e
	}
	for _, snapshot := range all {
		if snapshot.Attempt.Status != "RUNNING" {
			continue
		}
		e = s.Update(ctx, snapshot.ID, func(w *c.Workflow) ([]c.Action, error) {
			w.Attempt.Status = "INTERRUPTED"
			w.Attempt.ExpiresAt = time.Now()
			return nil, nil
		})
		if e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) Control(ctx context.Context, id, command string) error {
	return s.ControlOnce(ctx, id, command, 0)
}
func (s *Store) ControlOnce(ctx context.Context, id, command string, commentID int64) error {
	return s.Update(ctx, id, func(w *c.Workflow) ([]c.Action, error) {
		if commentID > 0 {
			for _, seen := range w.Commands {
				if seen == commentID {
					return nil, nil
				}
			}
			if len(w.Commands) >= 1000 {
				return nil, errors.New("M0 command budget exhausted")
			}
			w.Commands = append(w.Commands, commentID)
		}
		switch command {
		case "pause":
			if w.Control == c.Paused || w.Control == c.Pausing {
				return nil, nil
			}
			w.Control = c.Pausing
		case "cancel":
			w.Control = c.Pausing
			w.Reason = "cancel requested"
		case "takeover":
			w.Control = c.Pausing
			w.Reason = "human takeover requested"
		case "resume":
			if w.Control != c.Paused {
				return nil, errors.New("resume requires PAUSED")
			}
			w.Control = c.Reconciling
		default:
			return nil, errors.New("unsupported control")
		}
		w.Generation++
		return []c.Action{Action(*w, "gate", command, GatePayload{Verdict: c.Fail, Key: w.Key, Reason: command})}, nil
	})
}

// ConfirmStop cannot hide an in-flight or uncertain remote write.
func (s *Store) MarkStopped(ctx context.Context, id, attemptID string) error {
	return s.Update(ctx, id, func(w *c.Workflow) ([]c.Action, error) {
		if w.Attempt.ID != attemptID {
			return nil, c.ErrStale
		}
		if w.Attempt.Status == "RUNNING" {
			w.Attempt.Status = "INTERRUPTED"
		}
		return nil, nil
	})
}
func (s *Store) ConfirmStop(ctx context.Context, id string) error {
	return s.Update(ctx, id, func(w *c.Workflow) ([]c.Action, error) {
		if w.Control != c.Pausing {
			return nil, nil
		}
		if w.Attempt.Status == "RUNNING" {
			return nil, errors.New("local process stop has not been confirmed")
		}
		var n int
		if e := s.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='candidate' AND status IN ('sending','unknown')`, id).Scan(&n); e != nil {
			return nil, e
		}
		if n > 0 {
			return nil, errors.New("remote write reconciliation pending")
		}
		w.Attempt.Status = "CANCELLED"
		switch w.Reason {
		case "cancel requested":
			w.Control = c.Cancelled
		case "human takeover requested":
			w.Control = c.HumanOwned
		default:
			w.Control = c.Paused
		}
		return nil, nil
	})
}
