package orchestration

import (
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestValidatePlanTaskKindScopes(t *testing.T) {
	tests := []struct {
		name    string
		task    domain.Task
		wantErr bool
	}{
		{
			name: "legacy empty kind remains code",
			task: domain.Task{ID: "legacy", Description: "legacy task", Paths: []string{"calculator.go"}},
		},
		{
			name:    "code empty scope rejected",
			task:    domain.Task{ID: "code-empty", Kind: "code", Description: "code task"},
			wantErr: true,
		},
		{
			name:    "code dot scope rejected",
			task:    domain.Task{ID: "code-dot", Kind: "code", Description: "code task", Paths: []string{"."}},
			wantErr: true,
		},
		{
			name:    "code parent traversal rejected",
			task:    domain.Task{ID: "code-parent", Kind: "code", Description: "code task", Paths: []string{"../outside"}},
			wantErr: true,
		},
		{
			name: "verification empty scope allowed",
			task: domain.Task{ID: "verify-empty", Kind: "verification", Description: "verify task"},
		},
		{
			name: "analysis dot scope allowed",
			task: domain.Task{ID: "analysis-dot", Kind: "analysis", Description: "analysis task", Paths: []string{"."}},
		},
		{
			name:    "verification parent traversal rejected",
			task:    domain.Task{ID: "verify-parent", Kind: "verification", Description: "verify task", Paths: []string{"../outside"}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlan([]domain.Task{tt.task})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePlan() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
