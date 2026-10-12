package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPreservesOutputItemsAndSubmitsRawReport(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("missing bearer token")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"store": "false", "background": "false", "max_output_tokens": "64"} {
			if got := string(body[key]); got != want {
				t.Fatalf("%s=%s, want %s", key, got, want)
			}
		}
		if requests == 1 {
			var include []string
			if err := json.Unmarshal(body["include"], &include); err != nil || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
				t.Fatalf("missing reasoning include: %s", body["include"])
			}
			_, _ = fmt.Fprintln(w, `{"id":"resp-1","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"output":[{"type":"reasoning","id":"rs_1","summary":[]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}]}`)
			return
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body["input"], &items); err != nil {
			t.Fatal(err)
		}
		joined := string(body["input"])
		for _, fragment := range []string{`"type":"reasoning"`, `"type":"function_call"`, `"type":"function_call_output"`, `"call_id":"call_1"`} {
			if !strings.Contains(joined, fragment) {
				t.Fatalf("second input did not retain %s: %s", fragment, joined)
			}
		}
		if len(items) != 4 {
			t.Fatalf("got %d conversation items, want initial input plus 3 retained items", len(items))
		}
		_, _ = fmt.Fprintln(w, `{"id":"resp-2","status":"completed","usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11},"output":[{"type":"function_call","id":"fc_2","call_id":"call_2","name":"submit_report","arguments":"{\"ok\":true}"}]}`)
	}))
	defer server.Close()

	runner, err := NewRunner(Config{BaseURL: server.URL + "/v1/responses", Token: "test-token", Model: "test-model", MaxOutputTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	beforeRequests, usageEvents := 0, 0
	result, err := runner.Run(context.Background(), Request{
		Input: json.RawMessage(`"do work"`), MaxCalls: 1, MaxRequests: 2,
		BeforeRequest: func(context.Context) error { beforeRequests++; return nil },
		OnUsage: func(_ context.Context, usage Usage) error {
			usageEvents++
			if usage.TotalTokens == 0 {
				t.Fatal("completed response usage was not forwarded")
			}
			return nil
		},
		Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`), Handler: func(_ context.Context, name string, args json.RawMessage) (string, error) {
			if name != "lookup" || string(args) != `{"q":"x"}` {
				t.Fatalf("handler got %s %s", name, args)
			}
			return `{"value":1}`, nil
		}}},
		ReportSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Report) != `{"ok":true}` || result.Requests != 2 || result.Calls != 1 || requests != 2 || beforeRequests != 2 || usageEvents != 2 {
		t.Fatalf("unexpected result: %+v; requests=%d", result, requests)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 6 || result.Usage.TotalTokens != 16 {
		t.Fatalf("usage not accumulated: %+v", result.Usage)
	}
}

func TestIncompleteResponseNeverRunsTool(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, `{"id":"resp-1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}`)
	}))
	defer server.Close()
	runner, err := NewRunner(Config{BaseURL: server.URL, Token: "token", Model: "model", MaxOutputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), Request{Input: json.RawMessage(`"x"`), MaxCalls: 1, MaxRequests: 1,
		Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`), Handler: func(context.Context, string, json.RawMessage) (string, error) {
			calls++
			return "unexpected", nil
		}}},
	})
	if err == nil || calls != 0 {
		t.Fatalf("err=%v, calls=%d; incomplete response must be rejected before tools run", err, calls)
	}
}

func TestNewClientRequiresConfigurationBeforeDoctor(t *testing.T) {
	if _, err := NewClient(Config{BaseURL: "https://example.invalid/v1", Model: "model"}); err == nil {
		t.Fatal("expected missing token to be rejected")
	}
}

func TestReadCompletedSSEUsesOnlyCompletedResponse(t *testing.T) {
	stream := strings.NewReader("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\"}]}}\n\n")
	response, err := readCompletedSSE(stream)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "resp-1" || response.Status != "completed" || len(response.Output) != 1 || !strings.Contains(string(response.Output[0]), "reasoning") {
		t.Fatalf("unexpected completed response: %+v", response)
	}
}

func TestValidateJSONValueEnforcesRequiredAdditionalPropertiesAndItems(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		schema string
		valid  bool
	}{
		{"required missing", `{}`, `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`, false},
		{"extra property denied", `{"id":"x","extra":1}`, `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`, false},
		{"array item mismatch", `[1,"bad"]`, `{"type":"array","items":{"type":"integer"}}`, false},
		{"nested report valid", `{"id":"x","values":[1,2]}`, `{"type":"object","properties":{"id":{"type":"string"},"values":{"type":"array","items":{"type":"integer"}}},"required":["id","values"],"additionalProperties":false}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSchemaDefinition(json.RawMessage(test.schema))
			if err != nil {
				t.Fatal(err)
			}
			err = validateJSONValue(json.RawMessage(test.value), json.RawMessage(test.schema))
			if (err == nil) != test.valid {
				t.Fatalf("validateJSONValue returned %v, valid=%v", err, test.valid)
			}
		})
	}
}

func TestMaxRequestsStopsAfterConfiguredResponses(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = fmt.Fprintln(w, `{"id":"resp-1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}`)
	}))
	defer server.Close()
	runner, err := NewRunner(Config{BaseURL: server.URL, Token: "token", Model: "model", MaxOutputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	var toolCalls int
	_, err = runner.Run(context.Background(), Request{
		Input: json.RawMessage(`"x"`), MaxCalls: 2, MaxRequests: 1,
		Tools: []Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`), Handler: func(context.Context, string, json.RawMessage) (string, error) { toolCalls++; return "ok", nil }}},
	})
	if err == nil || requests != 1 || toolCalls != 0 {
		t.Fatalf("err=%v requests=%d toolCalls=%d; run must stop before tools when no request remains", err, requests, toolCalls)
	}
}
