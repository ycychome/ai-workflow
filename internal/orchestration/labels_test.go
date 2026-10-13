package orchestration

import (
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestWorkflowLabelsDescribePhaseAndStoppedControl(t *testing.T) {
	for _, tt := range []struct {
		phase, control string
		want           []string
	}{
		{"TRIAGE", "ACTIVE", []string{"需求分析"}},
		{"AWAITING_MAINTAINER", "ACTIVE", []string{"待维护者合并"}},
		{"NEEDS_INFO", "CANCELLED", []string{"需要补充信息", "已取消"}},
		{"REVIEWING", "PAUSED", []string{"审查中", "已暂停"}},
		{"DEVELOPING", "HUMAN_OWNED", []string{"开发中", "人工控制"}},
		{"MERGED", "COMPLETED", []string{"已合并", "已完成"}},
	} {
		t.Run(tt.phase+"/"+tt.control, func(t *testing.T) {
			labels := workflowLabels(domain.Workflow{Phase: tt.phase, Control: tt.control})
			if len(labels) != len(tt.want) {
				t.Fatalf("got %v", labels)
			}
			for n, name := range tt.want {
				if labels[n].Name != workflowLabelPrefix+name {
					t.Fatalf("got %v", labels)
				}
			}
			for _, label := range labels {
				if !strings.HasPrefix(label.Name, workflowLabelPrefix) || len(label.Color) != 6 || label.Description == "" {
					t.Fatalf("invalid managed label: %v", label)
				}
			}
		})
	}
}
