package orchestration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestPublishRetryRequiresExactCommitChanges(t *testing.T) {
	for _, name := range []string{"exact retry", "extra protected file", "extra deletion", "extra executable bit", "altered message"} {
		t.Run(name, func(t *testing.T) {
			e, ctx := safetyRegressionDB(t)
			w := domain.Workflow{ID: "retry-91", RepositoryID: 77, Repository: "acme/pilot", InstallationID: 42, Issue: 91, PR: 191, Phase: "DEVELOPING", Control: "ACTIVE", BaseBranch: "main", BaseSHA: "base-sha", HeadSHA: "base-sha", Branch: "codex/issue-91", Tasks: []domain.Task{{ID: "task", Status: "VALIDATED"}}}
			if err := e.Store.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
			w = mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
			d := publishActionData{Changes: []gh.FileChange{{Path: "calculator.go", Content: []byte("fixed"), Mode: "100644"}}, TaskIDs: []string{"task"}, Message: "Implement task", ExpectedParent: w.HeadSHA, BaseBranch: w.BaseBranch, BaseSHA: w.BaseSHA, Branch: w.Branch}
			attachPublishIntegrationFixture(t, ctx, e.Store, &w, &d, "tree-published-head")
			a := store.NewAction(w, "publish", d)
			base := map[string]sandbox.File{"calculator.go": {Data: []byte("old"), Mode: 0644}, "untouched": {Data: []byte("preserve"), Mode: 0644}}
			head := map[string]sandbox.File{"calculator.go": {Data: []byte("fixed"), Mode: 0644}, "untouched": {Data: []byte("preserve"), Mode: 0644}}
			message := d.Message + "\n\nAI Workflow Action: " + a.ID
			switch name {
			case "extra protected file":
				head[".github/workflows/pwn.yml"] = sandbox.File{Data: []byte("unexpected"), Mode: 0644}
			case "extra deletion":
				delete(head, "untouched")
			case "extra executable bit":
				f := head["untouched"]
				f.Mode = 0755
				head["untouched"] = f
			case "altered message":
				message = "external edit\n" + message
			}
			mock := newGateGitHubMock()
			pr := gateMockPR(w)
			pr.Head.SHA = "published-head"
			mock.setPR(pr, nil)
			mock.commits["published-head"] = gh.CommitInfo{SHA: "published-head", Message: message, TreeSHA: "tree-published-head", Parents: []string{w.HeadSHA}}
			archives := map[string][]byte{}
			for sha, files := range map[string]map[string]sandbox.File{"base-sha": base, "published-head": head} {
				var b bytes.Buffer
				zw := zip.NewWriter(&b)
				for path, f := range files {
					h := &zip.FileHeader{Name: "snapshot/" + path}
					h.SetMode(f.Mode)
					entry, err := zw.CreateHeader(h)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = entry.Write(f.Data); err != nil {
						t.Fatal(err)
					}
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
				archives[sha] = b.Bytes()
			}
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/zipball/") {
					sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/pilot/zipball/")
					_, _ = rw.Write(archives[sha])
					return
				}
				mock.ServeHTTP(rw, r)
			}))
			defer server.Close()
			e.app = safetyGitHubClient(t, server.URL, server.Client())
			e.cfg.Repositories = []config.Repository{{ID: 77, Name: "acme/pilot", AllowedPaths: []string{"calculator.go"}, ProtectedPaths: []string{".github/**"}}}
			_, err, unknown := e.publishGitHub(ctx, w, a, d)
			got := mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
			if name == "exact retry" {
				if err != nil || unknown || got.HeadSHA != "published-head" || got.Tasks[0].Status != "INTEGRATED" {
					t.Fatalf("exact retry failed: err=%v unknown=%v workflow=%+v", err, unknown, got)
				}
			} else if err == nil || unknown || got.HeadSHA != w.HeadSHA || got.Tasks[0].Status != "VALIDATED" {
				t.Fatalf("external change adopted: err=%v unknown=%v workflow=%+v", err, unknown, got)
			}
		})
	}
}

func TestReconcileRejectsResponsesPredatingNewApproval(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "reconcile-snapshot", RepositoryID: 77, Repository: "acme/pilot", InstallationID: 42, Issue: 192, Phase: "INTAKE", Control: "ACTIVE", IssueTitle: "old title", IssueBody: "old body"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	w = mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
	mock := newGateGitHubMock()
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/") {
			// A newer webhook and approval commit while the old GET is in flight.
			err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
				x.IssueTitle, x.IssueBody = "new title", "new body"
				x.Generation++
				x.Spec = domain.Spec{Title: "new spec", Approved: true, SourceHash: normalizedIssueSourceHash(x.IssueTitle, x.IssueBody)}
				x.Phase = "PLANNING"
				return nil, nil
			})
			if err != nil {
				t.Error(err)
			}
			writeGateJSON(rw, map[string]any{"number": w.Issue, "title": "old title", "body": "old body", "state": "open"})
			return
		}
		mock.ServeHTTP(rw, r)
	}))
	defer server.Close()
	e.app = safetyGitHubClient(t, server.URL, server.Client())
	e.cfg.Repositories = []config.Repository{{ID: 77, Name: "acme/pilot"}}
	if err := e.reconcileOne(ctx, w); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale response error=%v", err)
	}
	got := mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
	if got.IssueTitle != "new title" || got.Control != "ACTIVE" || got.Phase != "PLANNING" || !sourceMatchesIssue(got, got.Spec.SourceHash) {
		t.Fatalf("new approval overwritten: %+v", got)
	}
}

func TestIssueEditRejectsStaleSnapshotWithoutCancellingNewWorker(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "edited-snapshot", RepositoryID: 77, Repository: "acme/pilot", InstallationID: 42, Issue: 193, Phase: "INTAKE", Control: "ACTIVE", IssueTitle: "original"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	w = mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
	if err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		x.IssueTitle = "newer"
		x.Generation++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cancelled := false
	e.active[w.ID] = func() { cancelled = true }
	if err := e.issueEdited(ctx, w, &gh.Issue{Title: "older response"}); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale edit error=%v", err)
	}
	got := mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
	if cancelled || got.IssueTitle != "newer" || got.Control != "ACTIVE" {
		t.Fatalf("stale edit changed newer work: cancelled=%v workflow=%+v", cancelled, got)
	}
}

func TestPullRequestEventRetriesStaleResponseWithoutUndoingControl(t *testing.T) {
	for _, control := range []string{"HUMAN_OWNED", "COMPLETED"} {
		t.Run(control, func(t *testing.T) {
			e, ctx := safetyRegressionDB(t)
			w := domain.Workflow{ID: "pr-snapshot", RepositoryID: 77, Repository: "acme/pilot", InstallationID: 42, Issue: 194, PR: 294, Phase: "REVIEWING", Control: "ACTIVE", HeadSHA: "current-head", BaseSHA: "base-sha", BaseBranch: "main", Branch: "codex/issue-194"}
			if err := e.Store.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
			w = mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
			pr := gateMockPR(w)
			mock := newGateGitHubMock()
			var first sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/acme/pilot/pulls/294" {
					response := pr
					isOld := false
					first.Do(func() {
						isOld = true
						if err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
							x.Generation++
							x.Control = control
							if control == "COMPLETED" {
								x.Phase = "MERGED"
							}
							return nil, nil
						}); err != nil {
							t.Error(err)
						}
					})
					if isOld {
						response.State = "closed"
					} else if control == "COMPLETED" {
						response.State = "closed"
						response.Merged = true
					}
					writeGateJSON(rw, response)
					return
				}
				mock.ServeHTTP(rw, r)
			}))
			defer server.Close()
			e.app = safetyGitHubClient(t, server.URL, server.Client())
			e.cfg.Repositories = []config.Repository{{ID: 77, Name: "acme/pilot"}}
			payload, err := json.Marshal(gh.WebhookEvent{Action: "closed", PullRequest: &pr, Repository: &gh.WebhookRepository{ID: 77, FullName: "acme/pilot"}, Installation: &gh.WebhookInstallation{ID: 42}})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Store.Inbox(ctx, "stale-pr-delivery", "pull_request", payload); err != nil {
				t.Fatal(err)
			}
			claim, err := e.Store.ClaimInbox(ctx)
			if err != nil {
				t.Fatal(err)
			}
			processErr := e.handleInbox(ctx, claim)
			if !errors.Is(processErr, store.ErrStale) {
				t.Fatalf("stale PR error=%v", processErr)
			}
			if err = e.Store.FinishInbox(ctx, claim, processErr); err != nil {
				t.Fatal(err)
			}
			got := mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
			phase := "REVIEWING"
			if control == "COMPLETED" {
				phase = "MERGED"
			}
			if got.Control != control || got.Phase != phase || got.Generation != w.Generation+1 {
				t.Fatalf("old PR response overwrote current state: %+v", got)
			}
			var state string
			if err = e.Store.DB.QueryRow(ctx, "SELECT state FROM inbox WHERE id=$1", claim.ID).Scan(&state); err != nil || state != "retry_wait" {
				t.Fatalf("event lost: state=%s err=%v", state, err)
			}
			if _, err = e.Store.DB.Exec(ctx, "UPDATE inbox SET next_attempt=now() WHERE id=$1", claim.ID); err != nil {
				t.Fatal(err)
			}
			claim, err = e.Store.ClaimInbox(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.handleInbox(ctx, claim); err != nil {
				t.Fatalf("fresh retry failed: %v", err)
			}
			if err = e.Store.FinishInbox(ctx, claim, nil); err != nil {
				t.Fatal(err)
			}
			got = mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
			if got.Control != control || got.Phase != phase {
				t.Fatalf("retry undid current state: %+v", got)
			}
			if err = e.Store.DB.QueryRow(ctx, "SELECT state FROM inbox WHERE id=$1", claim.ID).Scan(&state); err != nil || state != "processed" {
				t.Fatalf("retry did not settle: state=%s err=%v", state, err)
			}
		})
	}
}

func TestResumeDoesNotUndoTakeoverAfterPrechecks(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "resume-snapshot", RepositoryID: 77, Repository: "local/pilot", Issue: 195, Phase: "BLOCKED", Control: "PAUSED", IssueTitle: "request", IssueBody: "body", Spec: domain.Spec{Approved: true, SourceHash: normalizedIssueSourceHash("request", "body")}}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	// Keep the command's final Update waiting while its earlier reads complete.
	tx, err := e.Store.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, "SELECT id FROM workflows WHERE id=$1 FOR UPDATE", w.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- e.Command(ctx, w.ID, "local-dashboard", "resume", true) }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err = e.Store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND query LIKE '%FOR UPDATE OF w%' AND cardinality(pg_blocking_pids(pid))>0)`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("resume returned before update window: %v", err)
		case <-deadline.C:
			t.Fatal("resume did not reach the locked update")
		case <-tick.C:
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE workflows SET control='HUMAN_OWNED',generation=generation+1,version=version+1,data=data||'{"control":"HUMAN_OWNED"}'::jsonb WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, store.ErrStale) {
			t.Fatalf("stale resume error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	got := mustGetPrePRWorkflow(t, ctx, e.Store, w.ID)
	if got.Control != "HUMAN_OWNED" {
		t.Fatalf("resume undid takeover: %+v", got)
	}
}

// attachPublishIntegrationFixture models the validated controller integration
// receipt that must exist before an outbox publication can be authorized.
func attachPublishIntegrationFixture(t *testing.T, ctx context.Context, s *store.Store, w *domain.Workflow, d *publishActionData, tree string) {
	t.Helper()
	d.IntegrationID = "integration-" + domain.Digest([]any{w.ID, w.Generation, d.ExpectedParent, tree})
	d.ExpectedTree = tree
	record := domain.IntegrationRecord{ID: d.IntegrationID, ParentSHA: d.ExpectedParent, TreeSHA: tree, Generation: w.Generation, SpecDigest: w.Spec.Digest, Status: "VALIDATED", CreatedAt: time.Now().UTC()}
	if err := s.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		x.IntegrationRecords = append(x.IntegrationRecords, record)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	*w = mustGetPrePRWorkflow(t, ctx, s, w.ID)
}
