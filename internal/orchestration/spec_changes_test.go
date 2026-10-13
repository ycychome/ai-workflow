package orchestration

import (
	"errors"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestDeletedTestsRequireAuthorizationBeforeVerification(t *testing.T) {
	base := map[string]sandbox.File{"sum.go": {Data: []byte("package sum")}, "sum_test.go": {Data: []byte("existing test")}}
	head := map[string]sandbox.File{"sum.go": {Data: []byte("package sum")}}
	changes := RiskChangesFromSnapshots(base, head)
	if len(changes) != 1 || changes[0].Status != "deleted" || changes[0].BeforeDigest == "" {
		t.Fatal("deleted test omitted from actual diff")
	}
	if err := RequireRiskAuthorization(domain.Workflow{}, changes, "key", time.Now()); !errors.Is(err, ErrRiskAuthorizationRequired) {
		t.Fatalf("test deletion bypassed pre-verification gate: %v", err)
	}
}
func TestRiskAuthorizationExactDiffKeyAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	changes := []domain.RiskPathChange{{Path: "auth.go", Status: "modified", BeforeDigest: "before", AfterDigest: "after"}}
	authorization := domain.RiskAuthorization{ID: "authorization", Actor: "maintainer", Reason: "已检查越权反例", VerificationKey: "key", Changes: changes, ChangesDigest: domain.Digest(changes), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	w := domain.Workflow{RiskAuthorizations: []domain.RiskAuthorization{authorization}}
	if err := RequireRiskAuthorization(w, changes, "key", now); err != nil {
		t.Fatal(err)
	}
	if err := RequireRiskAuthorization(w, changes, "other-key", now); !errors.Is(err, ErrRiskAuthorizationRequired) {
		t.Fatal("authorization reused across candidate identities")
	}
	if err := RequireRiskAuthorization(w, changes, "key", now.Add(time.Hour)); !errors.Is(err, ErrRiskAuthorizationRequired) {
		t.Fatal("expired authorization accepted")
	}
	mutated := append([]domain.RiskPathChange(nil), changes...)
	mutated[0].AfterDigest = "different-source"
	if err := RequireRiskAuthorization(w, mutated, "key", now); !errors.Is(err, ErrRiskAuthorizationRequired) {
		t.Fatal("authorization reused for changed source content")
	}
}
func TestRiskAuthorizationCannotAuthorizeCIPath(t *testing.T) {
	now := time.Now().UTC()
	changes := []domain.RiskPathChange{{Path: ".github/workflows/ci.yml", Status: "modified"}}
	w := domain.Workflow{RiskAuthorizations: []domain.RiskAuthorization{{Actor: "maintainer", Reason: "批准", VerificationKey: "key", Changes: changes, ChangesDigest: domain.Digest(changes), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}}
	if err := RequireRiskAuthorization(w, changes, "key", now); err == nil {
		t.Fatal("risk authorization enlarged CI write scope")
	}
}
func TestRiskClassifierCoversAuthMigrationAndTestRemoval(t *testing.T) {
	for _, name := range []string{"auth.go", "src/authentication.ts", "internal/permissions/check.go", "src/tenant.go", "migrations/001.sql", "db/schema/users.sql", "tests/spec.rs", "sum_test.go"} {
		risks, err := ClassifyRiskChanges([]domain.RiskPathChange{{Path: name, Status: "modified"}})
		if err != nil || len(risks) == 0 {
			t.Fatalf("missing risk for %s: %v", name, err)
		}
	}
	if err := RequireRiskAuthorization(domain.Workflow{}, []domain.RiskPathChange{{Path: "new_test.go", Status: "added"}}, "key", time.Now()); err != nil {
		t.Fatal("new test alone should not require deletion authorization")
	}
	if err := RequireRiskAuthorization(domain.Workflow{}, []domain.RiskPathChange{{Path: "normal.go", Status: "modified"}}, "key", time.Now()); err != nil {
		t.Fatal(err)
	}
}
func TestRenameCannotHideExistingTestRemoval(t *testing.T) {
	change := domain.RiskPathChange{Path: "unused.txt", PreviousPath: "test/test_login.py", Status: "renamed"}
	if err := RequireRiskAuthorization(domain.Workflow{}, []domain.RiskPathChange{change}, "key", time.Now()); !errors.Is(err, ErrRiskAuthorizationRequired) {
		t.Fatal("test rename bypassed authorization")
	}
}
