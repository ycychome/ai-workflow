package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

func maintenancePlan(repositoryID int64) RepositoryMaintenance {
	return RepositoryMaintenance{RepositoryID: repositoryID, Token: strings.Repeat("a", 64), OldEpoch: "legacy", NewEpoch: "reset-maintenance-1", PlanDigest: "approved-reset-plan-sha256"}
}

func TestRepositoryMaintenanceFencesAllDispatchWithoutStoppingOtherRepositories(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	for _, w := range []domain.Workflow{{ID: "maintained", RepositoryID: 99, Issue: 1, Phase: "DEVELOPING"}, {ID: "unrelated", RepositoryID: 100, Issue: 1, Phase: "DEVELOPING"}} {
		if _, err := s.CreateIntake(ctx, w, intakeTestComment); err != nil {
			t.Fatal(err)
		}
		enqueueTestAgent(t, s, w.ID+"-agent", w.ID)
		if err := s.Update(ctx, w.ID, func(current *domain.Workflow) ([]Action, error) {
			return []Action{NewAction(*current, "labels", map[string]string{"label": "status"})}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	plan := maintenancePlan(99)
	status, err := s.AcquireRepositoryMaintenance(ctx, plan)
	if err != nil || !status.Held || !status.IntakeStopped {
		t.Fatalf("acquire=%+v %v", status, err)
	}
	other := &Store{DB: s.DB}
	if _, err = other.CreateIntake(ctx, domain.Workflow{ID: "forbidden-intake", RepositoryID: 99, Issue: 2}, intakeTestComment); !errors.Is(err, ErrRepositoryMaintenance) {
		t.Fatalf("intake crossed maintenance fence: %v", err)
	}
	if _, err = other.CreateIfAbsent(ctx, domain.Workflow{ID: "legacy-forbidden", RepositoryID: 99, Issue: 3}); !errors.Is(err, ErrRepositoryMaintenance) {
		t.Fatalf("legacy create bypassed fence: %v", err)
	}
	if _, err = other.ClaimAgent(ctx, "maintained-agent", "worker", 4, 2); !errors.Is(err, ErrRepositoryMaintenance) {
		t.Fatalf("agent crossed maintenance fence: %v", err)
	}
	claimed, err := other.ClaimWithLimit(ctx, "other-controller", 4)
	if err != nil || claimed.RepositoryID != 100 {
		t.Fatalf("other repository workflow blocked: %+v %v", claimed, err)
	}
	if _, err = other.ClaimAgent(ctx, "unrelated-agent", "other-controller", 4, 2); err != nil {
		t.Fatalf("maintained fair queue head blocked another repository: %v", err)
	}
	action, err := other.ClaimAction(ctx)
	if err != nil || action.WorkflowID != "unrelated" {
		t.Fatalf("normal outbox crossed maintenance: %+v %v", action, err)
	}
	labels, err := other.ClaimLabelAction(ctx)
	if err != nil || labels.WorkflowID != "unrelated" {
		t.Fatalf("label outbox crossed maintenance: %+v %v", labels, err)
	}
	if _, err = other.ClaimAction(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("maintained normal action dispatched: %v", err)
	}
	if _, err = other.ClaimLabelAction(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("maintained label action dispatched: %v", err)
	}
	if err = s.ReleaseRepositoryMaintenance(ctx, plan, true); err != nil {
		t.Fatal(err)
	}
	status, err = s.RepositoryMaintenanceStatus(ctx, 99)
	if err != nil || status.Held || !status.IntakeStopped {
		t.Fatalf("release lost stopped marker: %+v %v", status, err)
	}
	if _, err = s.CreateIntake(ctx, domain.Workflow{ID: "still-stopped", RepositoryID: 99, Issue: 4}, intakeTestComment); !errors.Is(err, ErrRepositoryMaintenance) {
		t.Fatalf("release reopened intake: %v", err)
	}
	if err = s.ReleaseRepositoryMaintenance(ctx, plan, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateIntake(ctx, domain.Workflow{ID: "resumed-intake", RepositoryID: 99, Issue: 5}, intakeTestComment); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryMaintenanceRejectsWrongPlanAndWaitsForInflightAcknowledgement(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	w := domain.Workflow{ID: "inflight", RepositoryID: 99, Issue: 1, Phase: "DEVELOPING"}
	if _, err := s.CreateIntake(ctx, w, intakeTestComment); err != nil {
		t.Fatal(err)
	}
	action, err := s.ClaimAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.ClaimWithLimit(ctx, "active-controller", 4)
	if err != nil {
		t.Fatal(err)
	}
	plan := maintenancePlan(99)
	status, err := s.AcquireRepositoryMaintenance(ctx, plan)
	if err != nil || status.LiveWorkflowLeases != 1 || status.RunningOutbox != 1 {
		t.Fatalf("inflight counts=%+v %v", status, err)
	}
	if err = s.StartAction(ctx, action); !errors.Is(err, ErrRepositoryMaintenance) {
		t.Fatalf("claimed-before-lock action started after lock: %v", err)
	}
	wrong := plan
	wrong.PlanDigest = "changed-plan"
	if err = s.RetireRepositoryMaintenance(ctx, wrong); !errors.Is(err, ErrMaintenanceIdentity) {
		t.Fatalf("changed plan retired repository: %v", err)
	}
	wrong = plan
	wrong.Token = strings.Repeat("b", 64)
	if _, err = s.AcquireRepositoryMaintenance(ctx, wrong); !errors.Is(err, ErrMaintenanceIdentity) {
		t.Fatalf("another controller stole maintenance: %v", err)
	}
	if err = s.ReleaseRepositoryMaintenance(ctx, wrong, false); !errors.Is(err, ErrMaintenanceIdentity) {
		t.Fatalf("another token released maintenance: %v", err)
	}
	if err = s.RetireRepository(ctx, 99, plan.NewEpoch); !errors.Is(err, ErrMaintenanceIdentity) {
		t.Fatalf("legacy retirement bypassed token: %v", err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); !errors.Is(err, ErrRepositoryBusy) {
		t.Fatalf("inflight resources retired: %v", err)
	}
	// A known unsent action may be abandoned; an actual started request instead
	// requires FinishAction's truthful receipt before the reset can continue.
	if err = s.AbandonAction(ctx, action, "maintenance blocked external start"); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); !errors.Is(err, ErrRepositoryBusy) {
		t.Fatalf("live workflow lease retired: %v", err)
	}
	s.Release(ctx, lease)
	if err = s.RetireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	status, err = s.RepositoryMaintenanceStatus(ctx, 99)
	if err != nil || !status.Retired || status.Epoch != plan.NewEpoch || !status.Quiescent() {
		t.Fatalf("retirement status=%+v %v", status, err)
	}
	if err = s.ReleaseRepositoryMaintenance(ctx, plan, true); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatalf("retry of committed retirement failed: %v", err)
	}
}

func TestRepositoryMaintenanceFenceAcrossProcesses(t *testing.T) {
	if os.Getenv("AGENT_QUEUE_MAINTENANCE_CHILD") == "1" {
		s := agentQueueTestStore(t)
		ctx := context.Background()
		if _, err := s.CreateIntake(ctx, domain.Workflow{ID: "child-new", RepositoryID: 99, Issue: 2}, intakeTestComment); !errors.Is(err, ErrRepositoryMaintenance) {
			t.Fatalf("child intake bypassed fence: %v", err)
		}
		if _, err := s.ClaimAgent(ctx, "child-target", "child-controller", 4, 2); !errors.Is(err, ErrRepositoryMaintenance) {
			t.Fatalf("child agent bypassed fence: %v", err)
		}
		if _, err := s.ClaimWithLimit(ctx, "child-controller", 4); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("child workflow bypassed fence: %v", err)
		}
		if _, err := s.ClaimAction(ctx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("child outbox bypassed fence: %v", err)
		}
		return
	}
	s := agentQueueTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateIntake(ctx, domain.Workflow{ID: "child-workflow", RepositoryID: 99, Issue: 1, Phase: "DEVELOPING"}, intakeTestComment); err != nil {
		t.Fatal(err)
	}
	enqueueTestAgent(t, s, "child-target", "child-workflow")
	if _, err := s.AcquireRepositoryMaintenance(ctx, maintenancePlan(99)); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRepositoryMaintenanceFenceAcrossProcesses$", "-test.count=1")
	child.Env = append(os.Environ(), "AGENT_QUEUE_MAINTENANCE_CHILD=1", "AGENT_QUEUE_CHILD=1")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("independent controller failed maintenance fence: %v\n%s", err, output)
	}
}

func TestRepositoryMaintenanceResumesExactStoppedOperation(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	plan := maintenancePlan(99)
	if _, err := s.AcquireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseRepositoryMaintenance(ctx, plan, true); err != nil {
		t.Fatal(err)
	}
	if status, err := s.AcquireRepositoryMaintenance(ctx, plan); err != nil || !status.Held || status.Retired {
		t.Fatalf("resume before retire: %+v %v", status, err)
	}
	if err := s.RetireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseRepositoryMaintenance(ctx, plan, true); err != nil {
		t.Fatal(err)
	}
	wrong := plan
	wrong.PlanDigest = "different-plan"
	if _, err := s.AcquireRepositoryMaintenance(ctx, wrong); err == nil {
		t.Fatal("changed plan resumed retired operation")
	}
	status, err := s.AcquireRepositoryMaintenance(ctx, plan)
	if err != nil || !status.Held || !status.Retired || !status.IntakeStopped || status.Epoch != plan.NewEpoch {
		t.Fatalf("resume after retire: %+v %v", status, err)
	}
	if err := s.RetireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseRepositoryMaintenance(ctx, plan, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireRepositoryMaintenance(ctx, plan); err == nil {
		t.Fatal("old token reacquired after intake resumed")
	}
}
