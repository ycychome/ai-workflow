package orchestration

import (
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestClarificationOnlyResumesAuthorizedActiveNeedsInfo(t *testing.T) {
	w := domain.Workflow{Phase: "NEEDS_INFO", Control: "ACTIVE", AuthorizedBy: "maintainer"}
	if !resumeAfterClarification(w, true) {
		t.Fatal("maintainer reply should restart analysis")
	}
	if resumeAfterClarification(w, false) {
		t.Fatal("unauthorized or bot reply must not restart")
	}
	for _, control := range []string{"PAUSED", "PAUSING", "CANCELLED", "HUMAN_OWNED"} {
		copy := w
		copy.Control = control
		if resumeAfterClarification(copy, true) {
			t.Fatalf("reply resumed %s", control)
		}
	}
	for _, phase := range []string{"INTAKE", "SPEC_READY", "REVIEWING", "AWAITING_MAINTAINER"} {
		copy := w
		copy.Phase = phase
		if resumeAfterClarification(copy, true) {
			t.Fatalf("reply resumed %s", phase)
		}
	}
	w.AuthorizedBy = ""
	if resumeAfterClarification(w, true) {
		t.Fatal("reply must not authorize intake")
	}
}
