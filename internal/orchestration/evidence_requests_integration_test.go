package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// Opt-in: uses an isolated schema and never starts workers or external actions.
func TestCoordinationLedgerAndAuditAtomicPostgresIntegration(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("需要显式启用隔离 PostgreSQL 集成环境")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(os.Getenv("CONFIG_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("coordination_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	url, err := schemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := coordinationFixture()
	w.Repository = "local/pilot"
	w.Generation = 1
	w.Control = "ACTIVE"
	w.Phase = "REVIEWING"
	if err := s.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	update := func(x *domain.Workflow) (any, error) {
		r := domain.EvidenceRequest{ID: "request", Question: "检查真实证据", VerificationKey: "key", TargetRole: "reviewer", Status: "QUEUED"}
		x.EvidenceRequests = append(x.EvidenceRequests, r)
		return r, nil
	}
	if err := s.UpdateCoordination(ctx, w.ID, "evidence_request_created", update); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.EvidenceRequests) != 1 || current.EvidenceRequests[0].Status != "QUEUED" {
		t.Fatal("request ledger not persisted")
	}
	var auditCount int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind='evidence_request_created'`, w.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit=%d error=%v", auditCount, err)
	}
	beforeVersion := current.Version
	rejected := errors.New("拒绝当前操作")
	if err := s.UpdateCoordination(ctx, w.ID, "must_not_commit", func(x *domain.Workflow) (any, error) {
		x.EvidenceRequests[0].Status = "COMPLETED"
		return nil, rejected
	}); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	current, err = s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != beforeVersion || current.EvidenceRequests[0].Status != "QUEUED" {
		t.Fatal("rejected operation changed durable state")
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind='must_not_commit'`, w.ID).Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatalf("rollback audit=%d error=%v", auditCount, err)
	}
}
