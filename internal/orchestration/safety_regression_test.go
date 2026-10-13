package orchestration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func safetyRegressionDB(t *testing.T) (*Engine, context.Context) {
	return safetyRegressionDBWithTimeout(t, 60*time.Second)
}

func safetyRegressionDBWithTimeout(t *testing.T, timeout time.Duration) (*Engine, context.Context) {
	t.Helper()
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_INTEGRATION=1 in the controlled integration environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	cfg, err := config.Load(os.Getenv("CONFIG_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	url := cfg.DatabaseURL
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("continued_review_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	url, err = controlTestSchemaURL(url, schema)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	cfg.DataDir = t.TempDir()
	cfg.Runner.Mode = "fake"
	cfg.Runner.Image = "ai-workflow-runner:local"
	cfg.Runner.Reviewers = 2
	cfg.Runner.AttemptMinutes = 1
	cfg.Budget.MaxAttempts = 3
	e, err := New(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e, ctx
}
func TestSafetyRegressionInvalidSeverityCanPassGate(t *testing.T) {
	w := domain.Workflow{Control: "ACTIVE", Spec: domain.Spec{Approved: true, Criteria: []domain.Criterion{{ID: "AC1"}}}, Review: domain.Review{Key: "key", VerificationPassed: true, SkepticComplete: true, Obligations: []domain.Obligation{{ID: "O1", CriterionID: "AC1"}}}}
	report := map[string]any{
		"evidence_requests": []any{},
		"obligations":       []any{map[string]any{"id": "O1", "description": "Requirement", "criterion_id": "AC1", "result": "satisfied", "evidence": []string{"EV1"}}},
		"findings":          []any{map[string]any{"id": "F1", "severity": "HIGH", "category": "correctness", "path": "main.go", "line": 1, "description": "Wrong result on boundary", "expected": "Correct result", "actual": "Wrong result", "preconditions": "Boundary input", "evidence_type": "SOURCE_ANALYSIS", "evidence": []string{"EV1"}, "state": "CONFIRMED"}}, "summary": "Found blocking issue"}
	b, _ := json.Marshal(report)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]any{"id": "r1", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc1", "call_id": "c1", "name": "submit_report", "arguments": string(b)}}})
	}))
	defer server.Close()
	runner, err := agent.NewRunner(agent.Config{BaseURL: server.URL, Token: "fixture", Model: "fixture", MaxOutputTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	ev := []domain.Evidence{{ID: "EV1", Kind: "SOURCE_ANALYSIS"}}
	result, err := runner.Run(context.Background(), agent.Request{Input: json.RawMessage(`"review"`), ReportSchema: reviewSchema(), MaxRequests: 1, ValidateReport: func(out json.RawMessage) error {
		var rr reviewReport
		if e := json.Unmarshal(out, &rr); e != nil {
			return e
		}
		return mergeReport(&w, rr, []string{"O1"}, ev, false)
	}})
	if err == nil || !strings.Contains(err.Error(), "enum") || len(result.Report) != 0 {
		t.Fatalf("Responses report must reject enum before merging: report=%s err=%v", result.Report, err)
	}
}
func TestSafetyRegressionInvalidateBeyond200(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	oldest := domain.Workflow{ID: domain.ID(99, 1), RepositoryID: 99, Issue: 1, Repository: "local/pilot", Phase: "AWAITING_MAINTAINER", Control: "ACTIVE", PR: 1, PRReady: true, HeadSHA: "head", GateStatus: "completed", GateConclusion: "success", Review: domain.Review{Verdict: "PASS"}}
	if err := e.Store.Create(ctx, oldest); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 201; i++ {
		w := domain.Workflow{ID: domain.ID(99, i), RepositoryID: 99, Issue: i, Repository: "local/pilot", Phase: "INTAKE", Control: "ACTIVE"}
		if err := e.Store.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	all, err := e.Store.List(ctx)
	if err != nil || len(all) != 201 {
		t.Fatalf("background enumeration: len=%d err=%v", len(all), err)
	}
	recent, err := e.Store.ListRecent(ctx)
	if err != nil || len(recent) != 200 {
		t.Fatalf("UI enumeration: len=%d err=%v", len(recent), err)
	}
	if err := e.Invalidate(ctx, "model credential changed"); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.Get(ctx, oldest.ID)
	if err != nil {
		t.Fatal(err)
	}
	var gates int
	if err = e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='gate'", oldest.ID).Scan(&gates); err != nil {
		t.Fatal(err)
	}
	if got.Generation == 1 || got.Control == "ACTIVE" || gates == 0 {
		t.Fatalf("oldest workflow skipped: generation=%d control=%s verdict=%s revoke_actions=%d", got.Generation, got.Control, got.Review.Verdict, gates)
	}
}

func TestSafetyRegressionReadLargeEvidence(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: domain.ID(98, 1), RepositoryID: 98, Issue: 1, Repository: "local/pilot", Phase: "REVIEWING", Control: "ACTIVE"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	w, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := e.artifact(ctx, w, "peer-proof", "TOOL_VERIFIED", map[string]any{"stdout": strings.Repeat("x", 25000), "exit_code": 0})
	if err != nil {
		t.Fatal(err)
	}
	session := toolSession{e: e, w: w, workspace: &sandbox.Workspace{ID: "skeptic"}, readableEvidence: []domain.Evidence{original}}
	output, err := session.handler(ctx, "read_evidence", raw(map[string]string{"evidence_id": original.ID}))
	if err != nil {
		t.Fatal(err)
	}
	ev := session.listEvidence()
	var decoded struct {
		Content    string `json:"content"`
		Truncated  bool   `json:"truncated"`
		EvidenceID string `json:"evidence_id"`
		Notice     string `json:"notice"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil || !decoded.Truncated || decoded.EvidenceID == "" || decoded.Notice == "" || !strings.HasSuffix(decoded.Content, "\n[truncated]") {
		t.Fatalf("missing explicit bounded preview: %+v err=%v", decoded, err)
	}
	if len(ev) != 0 || decoded.EvidenceID != original.ID {
		t.Fatalf("reading must retain original proof identity without minting new evidence: count=%d id=%s", len(ev), decoded.EvidenceID)
	}
	if _, err := readRecordedEvidence(e.Config().DataDir, w.ID, original); err != nil {
		t.Fatalf("original evidence must remain valid and hash-verifiable: %v", err)
	}
	if !json.Valid([]byte(output)) {
		t.Fatalf("successful read_evidence returned invalid/empty JSON: output_bytes=%d new_evidence_records=%d", len(output), len(ev))
	}
}

func TestSafetyRegressionIssueIndentationIdentity(t *testing.T) {
	oldBody := "Required program:\n```python\nif enabled:\n    send()\nlog()\n```"
	newBody := "Required program:\n```python\nif enabled:\n    send()\n    log()\n```"
	if normalizedIssueSourceHash("Implement program", oldBody) == normalizedIssueSourceHash("Implement program", newBody) {
		t.Fatal("changed Python control flow has identical approved issue source identity")
	}
}
func TestSafetyRegressionSkepticDowngradeWithoutEvidence(t *testing.T) {
	w := domain.Workflow{Control: "ACTIVE", Spec: domain.Spec{Approved: true, Criteria: []domain.Criterion{{ID: "AC1"}}}, Evidence: []domain.Evidence{{ID: "EV1", Kind: "SOURCE_ANALYSIS", HeadSHA: "head"}}, HeadSHA: "head", Review: domain.Review{Key: "key", VerificationPassed: true, SkepticComplete: true, Obligations: []domain.Obligation{{ID: "O1", CriterionID: "AC1", Result: "satisfied", Evidence: []string{"EV1"}}}, Findings: []domain.Finding{{ID: "F1", Severity: "high", Description: "Wrong output", Expected: "Correct output", Actual: "Wrong output", Preconditions: "Boundary input", State: "CONFIRMED", EvidenceType: "TOOL_VERIFIED", Evidence: []string{"EV1"}}}}}
	f := w.Review.Findings[0]
	f.Severity = "low"
	f.State = "PROPOSED"
	f.EvidenceType = "MODEL_CLAIM"
	f.Evidence = nil
	report := reviewReport{Obligations: w.Review.Obligations, Findings: []domain.Finding{f}, Summary: "It is probably fine"}
	if err := mergeReport(&w, report, []string{"O1"}, nil, true); err == nil || !strings.Contains(err.Error(), "降低审查发现严重程度") {
		t.Fatalf("expected explicit severity downgrade rejection, got %v", err)
	}
	if pass, reason := domain.Gate(w, "key"); pass {
		t.Fatalf("blocking finding waived without any skeptic counterevidence: %s", reason)
	}
}

type safetyRegressionTransport func(*http.Request) (*http.Response, error)

func (f safetyRegressionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSafetyRegressionOutboxDeadline(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: domain.ID(96, 1), RepositoryID: 96, Issue: 1, Repository: "acme/pilot", InstallationID: 42, Phase: "INTAKE", Control: "ACTIVE"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) { return []store.Action{statusAction(*x)}, nil }); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	deadline := make(chan bool, 1)
	client := &http.Client{Transport: safetyRegressionTransport(func(r *http.Request) (*http.Response, error) {
		_, bounded := r.Context().Deadline()
		deadline <- bounded
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	e.app, err = gh.New(gh.Config{AppID: "711", BaseURL: "http://fixture.invalid", HTTPClient: client, PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.outboxLoop(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case bounded := <-deadline:
		if !bounded {
			t.Fatal("outbox remote request has no deadline despite a 90-second action lease; stalled request blocks the only publisher")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publisher never reached transport")
	}
}

func TestSafetyRegressionTimedOutEvidenceCanPassGate(t *testing.T) {
	ev := domain.Evidence{ID: "timeout-proof", Kind: "TOOL_VERIFIED", ExitCode: 124, HeadSHA: "head", Command: []string{"go", "test", "./..."}}
	w := domain.Workflow{Control: "ACTIVE", HeadSHA: "head", Spec: domain.Spec{Approved: true, Criteria: []domain.Criterion{{ID: "AC1"}}}, Evidence: []domain.Evidence{ev}, Review: domain.Review{Key: "key", VerificationPassed: true, SkepticComplete: true, Obligations: []domain.Obligation{{ID: "AC1", CriterionID: "AC1"}}}}
	report := reviewReport{Obligations: []domain.Obligation{{ID: "AC1", CriterionID: "AC1", Result: "satisfied", Evidence: []string{ev.ID}}}, Findings: []domain.Finding{}, Summary: "No findings"}
	if err := mergeReport(&w, report, []string{"AC1"}, []domain.Evidence{ev}, false); err != nil {
		return
	}
	if err := mergeReport(&w, report, []string{"AC1"}, nil, true); err != nil {
		return
	}
	if pass, reason := domain.Gate(w, "key"); pass {
		t.Fatalf("reviewer and skeptic accepted only timed-out tool evidence as satisfied: %s", reason)
	}
}

func TestReadEvidenceJSONPreviewBoundaries(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "evidence-boundaries", RepositoryID: 998765, Issue: 1, Repository: "local/pilot", Control: "ACTIVE", Phase: "REVIEWING"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	w, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 23980, 24000, 24001, 25000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			content := map[string]any{"stdout": strings.Repeat("x", size), "literal": "quotes: \" slash: \\ and unicode 界", "exit_code": 0}
			original, err := e.artifact(ctx, w, fmt.Sprintf("boundary-%d", size), "TOOL_VERIFIED", content)
			if err != nil {
				t.Fatal(err)
			}
			session := toolSession{e: e, w: w, workspace: &sandbox.Workspace{ID: fmt.Sprintf("boundary-%d", size)}, readableEvidence: []domain.Evidence{original}}
			output, err := session.handler(ctx, "read_evidence", raw(map[string]string{"evidence_id": original.ID}))
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Content    string `json:"content"`
				Truncated  bool   `json:"truncated"`
				Format     string `json:"content_format"`
				EvidenceID string `json:"evidence_id"`
			}
			if err := json.Unmarshal([]byte(output), &decoded); err != nil || decoded.Format != "json_text" || decoded.EvidenceID == "" {
				t.Fatalf("invalid encoded preview: %s err=%v", output, err)
			}
			if !decoded.Truncated && !json.Valid([]byte(decoded.Content)) {
				t.Fatal("complete preview is not valid JSON text")
			}
			if decoded.Truncated && !strings.HasSuffix(decoded.Content, "\n[truncated]") {
				t.Fatal("truncation is not explicit")
			}
			if len(session.listEvidence()) != 0 || decoded.EvidenceID != original.ID {
				t.Fatal("read preview created a derived proof alias")
			}
			if _, err := readRecordedEvidence(e.Config().DataDir, w.ID, original); err != nil {
				t.Fatalf("original evidence failed independent verification: %v", err)
			}
		})
	}
}
