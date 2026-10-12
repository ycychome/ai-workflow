package store

import (
	"context"
	"fmt"

	"github.com/ycychome/ai-workflow/internal/domain"
)

// Scheduling is read-only queue metadata for the dashboard. A workflow is
// reported running only while its persisted worker lease is currently valid.
type Scheduling struct {
	Status              string `json:"status"`
	Reason              string `json:"reason"`
	ReasonText          string `json:"reason_text"`
	BlockingWorkflowID  string `json:"blocking_workflow_id,omitempty"`
	BlockingIssue       int    `json:"blocking_issue,omitempty"`
	BlockingPR          int    `json:"blocking_pr,omitempty"`
	BlockingRepository  string `json:"blocking_repository,omitempty"`
	BlockingActionKind  string `json:"blocking_action_kind,omitempty"`
	BlockingActionState string `json:"blocking_action_state,omitempty"`
}

// SchedulingFor returns bounded dashboard scheduling facts for the requested
// workflows using the same global capacity and eligibility predicates as
// ClaimWithLimit.
func (s *Store) SchedulingFor(ctx context.Context, workflows []domain.Workflow, maxActive int) (map[string]Scheduling, error) {
	if maxActive < 1 || maxActive > 4 {
		return nil, fmt.Errorf("workflow concurrency must be between 1 and 4")
	}
	ids := make([]string, 0, len(workflows))
	for _, w := range workflows {
		ids = append(ids, w.ID)
	}
	result := make(map[string]Scheduling, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.DB.Query(ctx, `
WITH active_capacity AS (
  SELECT count(*)::int AS active FROM workflows
  WHERE lease_owner<>'' AND lease_until>now() AND control='ACTIVE'
    AND phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')
)
SELECT w.id,
       (w.lease_owner<>'' AND w.lease_until>now() AND w.control='ACTIVE'
         AND w.phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')) AS leased,
       (w.control='ACTIVE' AND w.phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')) AS runnable,
       active_capacity.active,
       COALESCE(outbox.kind,''), COALESCE(outbox.state,''),
       COALESCE(blocker.id,''), COALESCE(blocker.issue,0),
       COALESCE(blocker.pr,0), COALESCE(blocker.repository,'')
FROM workflows w
CROSS JOIN active_capacity
LEFT JOIN LATERAL (
  SELECT o.kind,o.state FROM outbox o
  WHERE o.workflow_id=w.id AND o.kind IN('publish','gate','ready')
    AND o.state IN('pending','running','unknown','retry_wait')
  ORDER BY CASE o.state WHEN 'running' THEN 0 WHEN 'unknown' THEN 1 WHEN 'pending' THEN 2 ELSE 3 END, o.created_at
  LIMIT 1
) outbox ON true
LEFT JOIN LATERAL (
  SELECT other.id,other.phase,other.issue,COALESCE((other.data->>'pr')::int,0) AS pr,
         COALESCE(other.data->>'repository','') AS repository
  FROM workflows other
  WHERE other.id<>w.id AND other.lease_owner<>'' AND other.lease_until>now()
    AND other.control='ACTIVE' AND other.phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')
  ORDER BY other.updated_at
  LIMIT 1
) blocker ON active_capacity.active >= $2
WHERE w.id=ANY($1::text[])`, ids, maxActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, actionKind, actionState, blockerID, blockerRepo string
		var leased, runnable bool
		var activeCapacity int
		var blockerIssue, blockerPR int
		if err := rows.Scan(&id, &leased, &runnable, &activeCapacity, &actionKind, &actionState, &blockerID, &blockerIssue, &blockerPR, &blockerRepo); err != nil {
			return nil, err
		}
		item := Scheduling{Status: "idle", Reason: "workflow_not_runnable", ReasonText: "当前阶段或控制状态不允许执行。"}
		switch {
		case leased:
			item.Status, item.Reason, item.ReasonText = "running", "lease_active", "执行器已领取此工作流，租约仍有效。"
		case !runnable:
			// Preserve the idle default.
		case actionKind != "":
			item.Status, item.Reason, item.ReasonText = "queued", "outbox_pending", "当前发布或门禁操作尚未完成，完成后继续排队。"
		case blockerID != "":
			item.Status, item.Reason, item.ReasonText = "queued", "parallel_capacity", fmt.Sprintf("全局并行工作流上限已满，工作流 #%d 正在运行。", blockerIssue)
		case runnable && activeCapacity >= maxActive:
			item.Status, item.Reason, item.ReasonText = "queued", "parallel_capacity", "全局并行工作流上限已满，当前工作流正在排队。"
		case runnable:
			item.Status, item.Reason, item.ReasonText = "queued", "waiting_for_worker", "工作流可运行，正在等待执行器领取。"
		}
		if blockerID != "" && runnable && !leased && actionKind == "" && activeCapacity >= maxActive {
			item.BlockingWorkflowID = blockerID
			item.BlockingIssue = blockerIssue
			item.BlockingPR = blockerPR
			item.BlockingRepository = blockerRepo
		}
		item.BlockingActionKind = actionKind
		item.BlockingActionState = actionState
		result[id] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, w := range workflows {
		if _, ok := result[w.ID]; !ok {
			return nil, fmt.Errorf("scheduling metadata missing for workflow %q", w.ID)
		}
	}
	return result, nil
}
