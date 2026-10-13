package orchestration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestDevelopmentPlannerInputIncludesControllerVerificationConfiguration(t *testing.T) {
	commands := [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}
	services := []string{"postgres"}
	w := domain.Workflow{
		PlanRevision: 3,
		BaseSHA:      "base-sha",
		HeadSHA:      "head-sha",
		Spec:         domain.Spec{Digest: "accepted-spec"},
		LastError:    "previous task failed",
	}
	input := developmentPlannerInput(w, commands, services, 4)
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ExpectedRevision     int        `json:"expected_revision"`
		BaseSHA              string     `json:"base_sha"`
		HeadSHA              string     `json:"head_sha"`
		LastError            string     `json:"last_error"`
		RemainingTaskCount   int        `json:"remaining_task_count"`
		VerificationCommands [][]string `json:"verification_commands"`
		VerificationServices []string   `json:"verification_services"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.VerificationCommands, commands) || !reflect.DeepEqual(decoded.VerificationServices, services) {
		t.Fatalf("verification config commands=%v services=%v", decoded.VerificationCommands, decoded.VerificationServices)
	}
	if decoded.ExpectedRevision != 3 || decoded.BaseSHA != "base-sha" || decoded.HeadSHA != "head-sha" || decoded.LastError != "previous task failed" || decoded.RemainingTaskCount != 4 {
		t.Fatalf("existing planner state was not preserved: %+v", decoded)
	}
}
