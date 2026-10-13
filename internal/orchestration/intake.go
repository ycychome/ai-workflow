package orchestration

import (
	"fmt"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
)

func issueIntake(repo config.Repository, installationID int64, issue gh.Issue, requireAccept bool) domain.Workflow {
	w := domain.Workflow{
		RepositoryEpoch: repo.EffectiveEpoch(),
		ID:              domain.ID(repo.ID, issue.Number), RepositoryID: repo.ID, Repository: repo.Name,
		InstallationID: installationID, Issue: issue.Number, IssueTitle: issue.Title,
		IssueBody: issue.Body, Phase: "INTAKE", Control: "ACTIVE", BaseBranch: repo.BaseBranch,
		Branch: fmt.Sprintf("codex/issue-%d", issue.Number),
	}
	w.Spec.SourceHash = normalizedIssueSourceHash(issue.Title, issue.Body)
	if !requireAccept {
		w.Phase = "TRIAGE"
		w.AuthorizedBy = "automatic-analysis"
	}
	return w
}
