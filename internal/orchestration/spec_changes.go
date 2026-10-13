package orchestration

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

var ErrRiskAuthorizationRequired = errors.New("实际差异包含高风险改动，必须由维护者明确授权后才能验证")

// ProposeWorkflowSpecChange persists the immutable proposal without modifying
// the approved spec. Existing pipeline code may instead call the domain helper
// inside its current fenced Save/Update transaction and refresh its snapshot.
func (e *Engine) ProposeWorkflowSpecChange(ctx context.Context, workflowID, actor string, spec domain.Spec) (domain.SpecChangeProposal, error) {
	var proposal domain.SpecChangeProposal
	err := e.Store.UpdateCoordination(ctx, workflowID, "spec_change_proposed", func(w *domain.Workflow) (any, error) {
		var err error
		proposal, err = domain.ProposeSpecChange(w, spec, actor, time.Now().UTC())
		return proposal, err
	})
	return proposal, err
}

// DecideWorkflowSpecChange applies the immutable decision and acceptance
// transition atomically. The root publisher may instead call domain.DecideSpecChange
// inside its own acceptance transaction after the same authorization and phase checks.
func (e *Engine) DecideWorkflowSpecChange(ctx context.Context, workflowID, proposalID, expectedDigest, actor, decision, reason string, dashboard ...bool) error {
	w, err := e.Store.Get(ctx, workflowID)
	if err != nil {
		return err
	}
	if !e.authorized(ctx, w, actor, len(dashboard) > 0 && dashboard[0]) {
		return errors.New("只有已授权的维护者可以决定规格变更")
	}
	return e.Store.Update(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), workflowID, func(current *domain.Workflow) ([]store.Action, error) {
		if current.Phase != "SPEC_READY" || (current.Control != "ACTIVE" && current.Control != "PAUSED") {
			return nil, errors.New("只能在规格待批准且未运行或可控制的阶段决定变更")
		}
		if err := domain.DecideSpecChange(current, proposalID, expectedDigest, decision, actor, reason, time.Now().UTC()); err != nil {
			return nil, err
		}
		if decision == "accepted" {
			current.Generation++
			current.Control = "ACTIVE"
			current.Phase = "PLANNING"
			current.AuthorizedBy = actor
			current.LastError = ""
			current.Review.Verdict = "OBSOLETE"
			current.ForceApproval = nil
		}
		return []store.Action{statusAction(*current)}, nil
	})
}

func riskTestPath(name string) bool {
	lower := strings.ToLower(name)
	base := path.Base(lower)
	return strings.Contains("/"+lower, "/tests/") || strings.Contains("/"+lower, "/test/") || strings.Contains("/"+lower, "/__tests__/") || strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasSuffix(base, "_test.py") || strings.HasSuffix(base, "test.java") || strings.HasSuffix(base, "tests.cs")
}
func normalizedRiskChanges(changes []domain.RiskPathChange) ([]domain.RiskPathChange, error) {
	out := append([]domain.RiskPathChange(nil), changes...)
	seen := map[string]bool{}
	for _, change := range out {
		if !safePath(change.Path) || seen[change.Path] {
			return nil, errors.New("高风险差异路径无效或重复")
		}
		seen[change.Path] = true
		switch change.Status {
		case "added", "modified", "deleted":
			if change.PreviousPath != "" {
				return nil, errors.New("只有 renamed 差异可以包含 previous_path")
			}
		case "renamed":
			if !safePath(change.PreviousPath) || change.PreviousPath == change.Path {
				return nil, errors.New("重命名前路径无效")
			}
		default:
			return nil, errors.New("差异必须明确标注 added、modified、deleted 或 renamed")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func ClassifyRiskChanges(changes []domain.RiskPathChange) ([]HighRiskChange, error) {
	normalized, err := normalizedRiskChanges(changes)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, change := range normalized {
		paths = append(paths, change.Path)
		if change.PreviousPath != "" {
			paths = append(paths, change.PreviousPath)
		}
	}
	risks, err := ClassifyHighRiskPaths(paths)
	if err != nil {
		return nil, err
	}
	for _, change := range normalized {
		if change.PreviousPath != "" {
			inherited, err := ClassifyRiskChanges([]domain.RiskPathChange{{Path: change.PreviousPath, Status: "deleted"}})
			if err != nil {
				return nil, err
			}
			risks = append(risks, inherited...)
		}
		if (riskTestPath(change.Path) && change.Status != "added") || riskTestPath(change.PreviousPath) {
			risks = append(risks, HighRiskChange{Path: change.Path, Category: "existing_tests", Reason: "删除、重命名或修改已有测试可能削弱验收，必须由维护者检查实际差异"})
		}
		lower := "/" + strings.ToLower(change.Path)
		if strings.HasPrefix(path.Base(lower), "auth") || strings.Contains(lower, "/authn") || strings.Contains(lower, "/authz") || strings.Contains(lower, "/authentication") || strings.Contains(lower, "/permission") || strings.Contains(lower, "/rbac") || strings.Contains(lower, "/acl/") {
			risks = append(risks, HighRiskChange{Path: change.Path, Category: "authorization", Reason: "认证或权限路径改动需要维护者明确授权"})
		}
		if strings.Contains(lower, "/migration") || strings.Contains(lower, "/schema/") {
			risks = append(risks, HighRiskChange{Path: change.Path, Category: "data_migration", Reason: "数据库结构或迁移改动需要维护者明确授权"})
		}
	}
	return risks, nil
}

// RiskChangesFromSnapshots classifies the actual controller-loaded base/head
// trees, including deleted tests that disappear from candidate file listings.
// Renames are conservatively represented as deletion plus addition.
func RiskChangesFromSnapshots(base, head map[string]sandbox.File) []domain.RiskPathChange {
	changes := []domain.RiskPathChange{}
	names := map[string]bool{}
	for name := range base {
		names[name] = true
	}
	for name := range head {
		names[name] = true
	}
	for name := range names {
		before, beforeOK := base[name]
		after, afterOK := head[name]
		beforeDigest, afterDigest := "", ""
		if beforeOK {
			beforeDigest = domain.Digest(before)
		}
		if afterOK {
			afterDigest = domain.Digest(after)
		}
		if beforeOK == afterOK && beforeDigest == afterDigest {
			continue
		}
		status := "modified"
		if !beforeOK {
			status = "added"
		}
		if !afterOK {
			status = "deleted"
		}
		changes = append(changes, domain.RiskPathChange{Path: name, Status: status, BeforeDigest: beforeDigest, AfterDigest: afterDigest})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}
func (e *Engine) CurrentRiskChanges(ctx context.Context, w domain.Workflow) ([]domain.RiskPathChange, error) {
	if w.BaseSHA == "" || w.HeadSHA == "" {
		return nil, errors.New("实际差异检查需要明确的基线和候选提交")
	}
	base, err := e.baseSnapshot(ctx, w)
	if err != nil {
		return nil, err
	}
	var head map[string]sandbox.File
	if w.TestedSHA != "" && w.TestedTreeSHA != "" {
		head, err = e.reviewSnapshot(ctx, w)
	} else {
		head, err = e.source(ctx, w, w.HeadSHA)
	}
	if err != nil {
		return nil, err
	}
	return RiskChangesFromSnapshots(base, head), nil
}
func riskScopeAllowed(e *Engine, w domain.Workflow, changes []domain.RiskPathChange) error {
	r := e.repository(w)
	for _, change := range changes {
		paths := []string{change.Path}
		if change.PreviousPath != "" {
			paths = append(paths, change.PreviousPath)
		}
		for _, name := range paths {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, ".github/workflows/") || strings.HasPrefix(lower, ".github/actions/") || !allowedPath(name, r, []string{"**"}) {
				return fmt.Errorf("路径 %s 受硬保护或超出允许范围；风险授权不能覆盖此限制", name)
			}
		}
	}
	return nil
}

// RequireRiskAuthorization must run before executing verification commands. A
// passing go test result cannot override removal of the tests it would execute.
// The caller must additionally enforce repository/task path scope independently.
func RequireRiskAuthorization(w domain.Workflow, changes []domain.RiskPathChange, key string, now time.Time) error {
	normalized, err := normalizedRiskChanges(changes)
	if err != nil {
		return err
	}
	for _, change := range normalized {
		for _, name := range []string{change.Path, change.PreviousPath} {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, ".github/workflows/") || strings.HasPrefix(lower, ".github/actions/") {
				return errors.New("CI 可信路径属于硬保护，风险授权不能放行")
			}
		}
	}
	risks, err := ClassifyRiskChanges(normalized)
	if err != nil {
		return err
	}
	if len(risks) == 0 {
		return nil
	}
	if key == "" {
		return ErrRiskAuthorizationRequired
	}
	digest := domain.Digest(normalized)
	for _, authorization := range w.RiskAuthorizations {
		if authorization.VerificationKey != key || authorization.ChangesDigest != digest || domain.Digest(authorization.Changes) != digest || strings.TrimSpace(authorization.Actor) == "" || strings.TrimSpace(authorization.Reason) == "" || authorization.CreatedAt.IsZero() || authorization.CreatedAt.After(now) || !authorization.ExpiresAt.After(now) {
			continue
		}
		return nil
	}
	paths := []string{}
	for _, risk := range risks {
		paths = append(paths, risk.Path)
	}
	return fmt.Errorf("%w：%s", ErrRiskAuthorizationRequired, strings.Join(paths, "、"))
}
func (e *Engine) CheckRiskAuthorization(ctx context.Context, w domain.Workflow, changes []domain.RiskPathChange) error {
	if err := riskScopeAllowed(e, w, changes); err != nil {
		return err
	}
	return RequireRiskAuthorization(w, changes, e.key(ctx, w), time.Now().UTC())
}

func (e *Engine) GrantRiskAuthorization(ctx context.Context, workflowID, actor, key, reason string, expiry time.Time, dashboard ...bool) (domain.RiskAuthorization, error) {
	var result domain.RiskAuthorization
	w, err := e.Store.Get(ctx, workflowID)
	if err != nil {
		return result, err
	}
	if !e.authorized(ctx, w, actor, len(dashboard) > 0 && dashboard[0]) {
		return result, errors.New("只有已授权的维护者可以批准高风险改动")
	}
	if !w.Spec.Approved || key == "" || e.key(ctx, w) != key {
		return result, errors.New("高风险授权必须绑定当前已批准规格的精确验证身份")
	}
	if strings.TrimSpace(reason) == "" || !expiry.After(time.Now()) {
		return result, errors.New("高风险授权必须提供具体理由和未来到期时间")
	}
	changes, err := e.CurrentRiskChanges(ctx, w)
	if err != nil {
		return result, err
	}
	if err = riskScopeAllowed(e, w, changes); err != nil {
		return result, err
	}
	risks, err := ClassifyRiskChanges(changes)
	if err != nil {
		return result, err
	}
	if len(risks) == 0 {
		return result, errors.New("当前实际差异没有需要此类授权的高风险改动")
	}
	err = e.Store.UpdateCoordination(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), workflowID, "risk_authorization_granted", func(current *domain.Workflow) (any, error) {
		if current.BaseSHA != w.BaseSHA || current.Spec.Digest != w.Spec.Digest || current.Generation != w.Generation || e.key(ctx, *current) != key {
			return nil, store.ErrStale
		}
		if !expiry.After(time.Now()) {
			return nil, errors.New("高风险授权在保存前已经到期")
		}
		result = domain.RiskAuthorization{VerificationKey: key, Changes: changes, ChangesDigest: domain.Digest(changes), Actor: actor, Reason: reason, CreatedAt: time.Now().UTC(), ExpiresAt: expiry}
		result.ID = "RA-" + domain.Digest(result)[:32]
		current.RiskAuthorizations = append(current.RiskAuthorizations, result)
		return result, nil
	})
	return result, err
}
func (e *Engine) HandleRiskAuthorizationCommand(ctx context.Context, workflowID, actor, command string, dashboard ...bool) (bool, error) {
	command = strings.TrimSpace(command)
	if len(command) >= 4 && strings.EqualFold(command[:4], "/ai ") {
		command = strings.TrimSpace(command[4:])
	}
	fields := strings.Fields(command)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "authorize-risk") {
		return false, nil
	}
	if len(fields) < 4 {
		return true, errors.New("用法：/ai authorize-risk <verification_key> <RFC3339到期时间> <理由>")
	}
	expiry, err := time.Parse(time.RFC3339, fields[2])
	if err != nil {
		return true, errors.New("授权到期时间必须使用 RFC3339 格式")
	}
	_, err = e.GrantRiskAuthorization(ctx, workflowID, actor, fields[1], strings.Join(fields[3:], " "), expiry, dashboard...)
	return true, err
}
