package orchestration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ycychome/ai-workflow/internal/domain"
)

type resolutionProposal struct {
	runID, findingID, kind, argument string
	proof                            []domain.Evidence
}

func resolutionProposals(task domain.Task, res buildResult) []resolutionProposal {
	var report struct {
		Summary   string `json:"summary"`
		Rebuttals []struct {
			FindingID string   `json:"finding_id"`
			Argument  string   `json:"argument"`
			Evidence  []string `json:"evidence"`
		} `json:"rebuttals"`
	}
	if json.Unmarshal(res.report, &report) != nil {
		return nil
	}
	proofs := map[string]domain.Evidence{}
	for _, ev := range res.evidence {
		if ev.Kind == "SOURCE_ANALYSIS" || ev.Kind == "TOOL_VERIFIED" {
			proofs[ev.ID] = ev
		}
	}
	proposals := []resolutionProposal{}
	if len(res.changes) > 0 {
		for _, findingID := range task.FindingIDs {
			proposals = append(proposals, resolutionProposal{runID: res.runID, findingID: findingID, kind: "fix", argument: report.Summary})
		}
	}
	for _, rebuttal := range report.Rebuttals {
		assigned := false
		for _, findingID := range task.FindingIDs {
			if findingID == rebuttal.FindingID {
				assigned = true
			}
		}
		if !assigned {
			continue
		}
		proof := []domain.Evidence{}
		for _, id := range rebuttal.Evidence {
			if ev, ok := proofs[id]; ok {
				proof = append(proof, ev)
			}
		}
		proposals = append(proposals, resolutionProposal{runID: res.runID, findingID: rebuttal.FindingID, kind: "rebuttal", argument: rebuttal.Argument, proof: proof})
	}
	return proposals
}
func (e *Engine) persistResolutionProposals(ctx context.Context, w *domain.Workflow, proposals []resolutionProposal) error {
	for _, proposal := range proposals {
		if _, err := e.RecordFindingResolution(ctx, w.ID, w.Review.Key, proposal.runID, proposal.findingID, proposal.kind, proposal.argument, proposal.proof); err != nil {
			return err
		}
	}
	if len(proposals) > 0 {
		current, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			return err
		}
		*w = current
	}
	return nil
}

func (e *Engine) verifyResolutionReport(ctx context.Context, w *domain.Workflow, runID string, report reviewReport, proof []domain.Evidence) error {
	for _, resolution := range w.FindingResolutions {
		if resolution.State != "PROPOSED" || resolution.VerificationKey != w.Review.Key {
			continue
		}
		for _, finding := range report.Findings {
			if finding.ID != resolution.FindingID {
				continue
			}
			decision := "disputed"
			switch finding.State {
			case "RESOLVED", "REFUTED":
				decision = "accepted"
			case "CONFIRMED":
				decision = "rejected"
			}
			selected := []domain.Evidence{}
			for _, id := range finding.Evidence {
				for _, ev := range proof {
					if ev.ID == id {
						selected = append(selected, ev)
					}
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("发现 %s 的处置缺少本次独立复核证据", finding.ID)
			}
			if err := e.VerifyFindingResolution(ctx, w.ID, w.Review.Key, resolution.ID, runID, decision, selected); err != nil {
				return err
			}
		}
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return err
	}
	*w = current
	return nil
}

// Publication binds a proposed fix to the actual integrated candidate, retaining
// the original finding identity separately. A model cannot perform this step.
func bindPublishedResolutions(w *domain.Workflow, taskIDs []string, sha, key string) {
	for i := range w.FindingResolutions {
		r := &w.FindingResolutions[i]
		if r.State != "PROPOSED" || r.Kind != "fix" {
			continue
		}
		for _, id := range taskIDs {
			if r.TaskID == id {
				if r.SourceVerificationKey == "" {
					r.SourceVerificationKey = r.VerificationKey
				}
				r.CandidateSHA = sha
				r.VerificationKey = key
			}
		}
	}
}
