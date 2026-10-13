package orchestration

import (
	"testing"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
)

func TestAutomaticAnalysisDoesNotApproveDevelopment(t *testing.T) {
	repo := config.Repository{ID: 12, Name: "example/pilot", BaseBranch: "main"}
	issue := gh.Issue{Number: 7, Title: "web example", Body: "show a web example"}
	for _, requireAccept := range []bool{false, true} {
		w := issueIntake(repo, 33, issue, requireAccept)
		if w.Spec.Approved || len(w.Tasks) > 0 || w.ModelCalls != 0 {
			t.Fatal("intake must not authorize development or fabricate execution")
		}
		if requireAccept && (w.Phase != "INTAKE" || domain.Runnable(w)) {
			t.Fatal("acceptance-required intake must wait")
		}
		if !requireAccept && (w.Phase != "TRIAGE" || !domain.Runnable(w)) {
			t.Fatal("default intake should run analysis")
		}
	}
}
