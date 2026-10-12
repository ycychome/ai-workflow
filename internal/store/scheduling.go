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
// workflows. Its runnable, outbox, lease and repository-blocker predicates
// intentionally match Store.Claim.
func (s *Store) SchedulingFor(ctx context.Context, workflows []domain.Workflow) (map[string]Scheduling, error) {
	ids := make([]string, 0, len(workflows))
	for _, w := range workflows {
		ids = append(ids, w.ID)
	}
	result := make(map[string]Scheduling, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.DB.Query(ctx, `
SELECT w.id,
       (w.lease_owner<>'' AND w.lease_until>now()) AS leased,
       (w.control='ACTIVE' AND w.phase IN('TRIAGE','PLANNING','DEVELOPING','VERIFYING','REVIEWING','CHANGES_REQUESTED')) AS runnable,
       COALESCE(outbox.kind,''), COALESCE(outbox.state,''),
       COALESCE(blocker.id,''), COALESCE(blocker.phase,''),
       COALESCE(blocker.leased,false),
       COALESCE(blocker.issue,0), COALESCE(blocker.pr,0), COALESCE(blocker.repository,'')
FROM workflows w
LEFT JOIN LATERAL (
  SELECT o.kind,o.state FROM outbox o
  WHERE o.workflow_id=w.id AND o.kind IN('publish','gate','ready')
    AND o.state IN('pending','running','unknown','retry_wait')
  ORDER BY CASE o.state WHEN 'running' THEN 0 WHEN 'unknown' THEN 1 WHEN 'pending' THEN 2 ELSE 3 END, o.created_at
  LIMIT 1
) outbox ON true
LEFT JOIN LATERAL (
  SELECT other.id,other.phase,other.issue,(other.lease_until>now()) AS leased,COALESCE((other.data->>'pr')::int,0) AS pr,
         COALESCE(other.data->>'repository','') AS repository
  FROM workflows other
  WHERE other.repository_id=w.repository_id AND other.id<>w.id
    AND (other.lease_until>now()
      OR (other.phase='AWAITING_MAINTAINER' AND other.control NOT IN('CANCELLED','CANCELLING','COMPLETED')))
  ORDER BY CASE WHEN other.lease_until>now() THEN 0 ELSE 1 END, other.updated_at
  LIMIT 1
) blocker ON true
WHERE w.id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, actionKind, actionState, blockerID, blockerPhase, blockerRepo string
		var leased, runnable, blockerLeased bool
		var blockerIssue, blockerPR int
		if err := rows.Scan(&id, &leased, &runnable, &actionKind, &actionState, &blockerID, &blockerPhase, &blockerLeased, &blockerIssue, &blockerPR, &blockerRepo); err != nil {
			return nil, err
		}
		item := Scheduling{Status: "idle", Reason: "workflow_not_runnable", ReasonText: "当前阶段或控制状态不允许执行。"}
		switch {
		case leased:
			item.Status, item.Reason, item.ReasonText = "running", "lease_active", "执行器已领取此工作流，租约仍有效。"
		case !runnable:
			// Preserve the idle default.
		case blockerID != "" && !blockerLeased && blockerPhase == "AWAITING_MAINTAINER":
			item.Status, item.Reason, item.ReasonText = "queued", "awaiting_maintainer", fmt.Sprintf("同仓库 Issue #%d 正在等待维护者处理。", blockerIssue)
		case blockerID != "":
			item.Status, item.Reason, item.ReasonText = "queued", "same_repository_running", fmt.Sprintf("同仓库 Issue #%d 正在运行，当前工作流排队中。", blockerIssue)
		case actionKind != "":
			item.Status, item.Reason, item.ReasonText = "queued", "outbox_pending", "当前发布或门禁操作尚未完成，完成后继续排队。"
		case runnable:
			item.Status, item.Reason, item.ReasonText = "queued", "waiting_for_worker", "工作流可运行，正在等待执行器领取。"
		}
		if blockerID != "" {
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
