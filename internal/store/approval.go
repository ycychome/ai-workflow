package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// Record the AI's decision in the same transaction as the accepted TaskSpec.
// The workflow row lock makes this deduplication safe across control processes.
func recordAISpecApproval(ctx context.Context, tx pgx.Tx, w domain.Workflow) error {
	if w.Phase != "PLANNING" || !w.Spec.Approved || w.Spec.ApprovedBy != "ai:dev-triage" {
		return nil
	}
	b, err := json.Marshal(map[string]any{"actor": w.Spec.ApprovedBy, "generation": w.Generation, "spec_digest": w.Spec.Digest, "source_hash": w.Spec.SourceHash, "revision": w.Spec.Revision, "criteria": w.Spec.Criteria})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit(workflow_id,kind,data) SELECT $1,'ai_spec_approved',$2::jsonb
 WHERE NOT EXISTS(SELECT 1 FROM audit WHERE workflow_id=$1 AND kind='ai_spec_approved' AND data->>'spec_digest'=$3 AND data->>'generation'=$4)`, w.ID, b, w.Spec.Digest, fmt.Sprint(w.Generation))
	return err
}
