package config

import (
	"testing"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestEvaluationConfigFreezesIndependentToolCallCap(t *testing.T) {
	cfg := Config{}
	frozen := cfg.EvaluationExecutionConfig()
	runner, ok := frozen["runner"].(map[string]any)
	if !ok || runner["max_tool_calls_per_run"] != agent.MaxToolCallsPerRun {
		t.Fatalf("runner config omitted independent tool cap: %#v", frozen["runner"])
	}
	modified := map[string]any{
		"model_profile": frozen["model_profile"],
		"budget":        frozen["budget"],
		"runner":        map[string]any{},
	}
	for key, value := range runner {
		modified["runner"].(map[string]any)[key] = value
	}
	modified["runner"].(map[string]any)["max_tool_calls_per_run"] = agent.MaxToolCallsPerRun + 1
	if domain.Digest(frozen) == domain.Digest(modified) {
		t.Fatal("changing the independent tool cap did not change the frozen evaluation digest")
	}
}
