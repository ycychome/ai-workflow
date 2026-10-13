package agent

import (
	"encoding/json"
	"testing"
)

func TestSchemaEnumEnforcedRecursively(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["critical","high","medium","low","info"]},"state":{"type":"string","enum":["PROPOSED","CONFIRMED"]},"evidence_type":{"type":"string","enum":["SOURCE_ANALYSIS","TOOL_VERIFIED"]}},"required":["severity","state","evidence_type"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false}`)
	if err := validateSchemaDefinition(schema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		severity, state, kind string
		valid                 bool
	}{
		{"high", "CONFIRMED", "TOOL_VERIFIED", true}, {"HIGH", "CONFIRMED", "TOOL_VERIFIED", false}, {"high", "confirmed", "TOOL_VERIFIED", false}, {"high", "FIX_CANDIDATE", "TOOL_VERIFIED", false}, {"high", "CONFIRMED", "tool_verified", false},
	} {
		payload, _ := json.Marshal(map[string]any{"findings": []any{map[string]any{"severity": tc.severity, "state": tc.state, "evidence_type": tc.kind}}})
		if err := validateJSONValue(payload, schema); (err == nil) != tc.valid {
			t.Fatalf("%s: valid=%v err=%v", payload, tc.valid, err)
		}
	}
}
func TestSchemaEnumDefinitionAndJSONEquality(t *testing.T) {
	for _, schema := range []string{`{"type":"string","enum":[]}`, `{"type":"string","enum":null}`, `{"type":"string","enum":"high"}`, `{"type":"string","enum":["high","high"]}`, `{"type":"string","enum":[3]}`, `{"type":"number","enum":[1,1.0]}`} {
		if err := validateSchemaDefinition(json.RawMessage(schema)); err == nil {
			t.Fatalf("invalid enum accepted: %s", schema)
		}
	}
	for _, tc := range []struct{ schema, value string }{{`{"type":"number","enum":[1]}`, `1e0`}, {`{"type":["null","string"],"enum":[null,"high"]}`, `null`}, {`{"type":"object","properties":{"n":{"type":"number"}},"enum":[{"n":1}]}`, `{"n":1.0}`}} {
		if err := validateSchemaDefinition(json.RawMessage(tc.schema)); err != nil {
			t.Fatal(err)
		}
		if err := validateJSONValue(json.RawMessage(tc.value), json.RawMessage(tc.schema)); err != nil {
			t.Fatal(err)
		}
	}
}
