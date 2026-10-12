package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Tool is an agent-callable function. Parameters must be a JSON Schema object.
// Handler returns the text placed in the function_call_output item.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Handler     func(context.Context, string, json.RawMessage) (string, error)
}

// Request is one bounded agent run. Input is any JSON value accepted by the
// Responses API input field, commonly a JSON string or an array of input items.
type Request struct {
	System        string
	Input         json.RawMessage
	Model         string
	Tools         []Tool
	ReportSchema  json.RawMessage
	MaxCalls      int
	MaxRequests   int
	BeforeRequest func(context.Context) error
	OnUsage       func(context.Context, Usage) error
}

// Usage contains the token totals reported by each completed response.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// Result is returned only when the model has submitted a valid JSON report.
type Result struct {
	Report     json.RawMessage
	Usage      Usage
	Calls      int // Number of ordinary tool handlers executed.
	Requests   int // Number of Responses API requests completed.
	ResponseID string
}

// Runner executes one agent loop using the Responses API.
type Runner struct {
	client          *Client
	maxOutputTokens int
}

// NewRunner validates endpoint configuration and the mandatory output bound.
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.MaxOutputTokens <= 0 {
		return nil, errors.New("agent: Config.MaxOutputTokens must be positive")
	}
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Runner{client: c, maxOutputTokens: cfg.MaxOutputTokens}, nil
}

// Doctor checks the configured Responses API endpoint with a small real
// request. It returns a configuration error without making a request whenever
// the endpoint, token, or model is missing.
func (r *Runner) Doctor(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("agent: nil Runner")
	}
	return r.client.Doctor(ctx)
}

// Run calls the model until it invokes submit_report. Tool calls are handled
// serially and only after a complete response has been received. The call
// budget also bounds API requests: at most MaxCalls+1 responses are requested.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	var result Result
	if r == nil || r.client == nil {
		return result, errors.New("agent: nil Runner")
	}
	if req.MaxCalls < 0 {
		return result, errors.New("agent: Request.MaxCalls cannot be negative")
	}
	if req.MaxRequests <= 0 {
		return result, errors.New("agent: Request.MaxRequests must be positive")
	}
	inputItems, err := normalizeInput(req.Input)
	if err != nil {
		return result, err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = r.client.model
	}
	apiTools, handlers, toolSchemas, reportSchema, err := prepareTools(req.Tools, req.ReportSchema)
	if err != nil {
		return result, err
	}
	for requestNo := 0; requestNo < req.MaxRequests; requestNo++ {
		input, err := json.Marshal(inputItems)
		if err != nil {
			return result, fmt.Errorf("agent: encode conversation input: %w", err)
		}
		if req.BeforeRequest != nil {
			if err := req.BeforeRequest(ctx); err != nil {
				return result, fmt.Errorf("agent: before request hook: %w", err)
			}
		}
		result.Requests++
		response, err := r.client.createResponse(ctx, responseRequest{
			Model: model, Instructions: req.System, Input: input,
			Tools: apiTools, MaxOutputTokens: r.maxOutputTokens,
			Store: false, Background: false,
		})
		if err != nil {
			return result, err
		}
		result.ResponseID = response.ID
		result.Usage.InputTokens += response.Usage.InputTokens
		result.Usage.OutputTokens += response.Usage.OutputTokens
		result.Usage.TotalTokens += response.Usage.TotalTokens
		if req.OnUsage != nil {
			if err := req.OnUsage(ctx, response.Usage); err != nil {
				return result, fmt.Errorf("agent: usage hook: %w", err)
			}
		}
		if len(response.Output) == 0 {
			return result, errors.New("agent: completed response contained no output items")
		}

		calls := make([]functionCallItem, 0)
		for _, raw := range response.Output {
			var item functionCallItem
			if err := json.Unmarshal(raw, &item); err != nil {
				return result, fmt.Errorf("agent: decode output item: %w", err)
			}
			if item.Type == "function_call" {
				if err := (&item).validate(); err != nil {
					return result, err
				}
				calls = append(calls, item)
			}
		}
		if len(calls) == 0 {
			return result, errors.New("agent: completed response did not call submit_report")
		}
		reportCount := 0
		for _, call := range calls {
			if call.Name == "submit_report" {
				reportCount++
			}
		}
		if reportCount > 0 {
			if reportCount != 1 || len(calls) != 1 {
				return result, errors.New("agent: submit_report must be the only function call in its response")
			}
			if err := validateJSONValue(calls[0].Arguments, reportSchema); err != nil {
				return result, fmt.Errorf("agent: submitted report does not match ReportSchema: %w", err)
			}
			result.Report = append(json.RawMessage(nil), calls[0].Arguments...)
			return result, nil
		}
		if result.Calls+len(calls) > req.MaxCalls {
			return result, fmt.Errorf("agent: tool call limit exceeded (%d)", req.MaxCalls)
		}
		for _, call := range calls {
			if _, ok := handlers[call.Name]; !ok {
				return result, fmt.Errorf("agent: model called unknown tool %q", call.Name)
			}
			if err := validateJSONValue(call.Arguments, toolSchemas[call.Name]); err != nil {
				return result, fmt.Errorf("agent: arguments for tool %q do not match its schema: %w", call.Name, err)
			}
		}
		if requestNo+1 >= req.MaxRequests {
			return result, fmt.Errorf("agent: request limit exhausted (%d) before tool execution", req.MaxRequests)
		}

		// Keep all output items exactly as received, including reasoning and
		// non-text items, so the next request has the full prior context.
		inputItems = append(inputItems, response.Output...)
		for _, call := range calls {
			handler := handlers[call.Name]
			output, callErr := handler(ctx, call.Name, call.Arguments)
			result.Calls++
			if callErr != nil {
				encoded, _ := json.Marshal(map[string]string{"error": callErr.Error()})
				output = string(encoded)
			}
			outputItem, err := json.Marshal(functionOutputItem{
				Type: "function_call_output", CallID: call.CallID, Output: string(output),
			})
			if err != nil {
				return result, fmt.Errorf("agent: encode tool output: %w", err)
			}
			inputItems = append(inputItems, outputItem)
		}
	}
	return result, fmt.Errorf("agent: request limit exceeded (%d)", req.MaxRequests)
}

func normalizeInput(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("agent: Request.Input must contain valid JSON")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
			return nil, errors.New("agent: Request.Input array must contain at least one item")
		}
		return items, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("agent: decode Request.Input: %w", err)
	}
	if _, ok := value.(string); ok {
		message, _ := json.Marshal(map[string]any{"role": "user", "content": value})
		return []json.RawMessage{message}, nil
	}
	return []json.RawMessage{append(json.RawMessage(nil), raw...)}, nil
}

func prepareTools(tools []Tool, reportSchema json.RawMessage) ([]apiTool, map[string]func(context.Context, string, json.RawMessage) (string, error), map[string]json.RawMessage, json.RawMessage, error) {
	if len(reportSchema) == 0 {
		reportSchema = json.RawMessage(`{"type":"object","additionalProperties":true}`)
	}
	if err := validateSchemaDefinition(reportSchema); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("agent: invalid ReportSchema: %w", err)
	}
	api := make([]apiTool, 0, len(tools)+1)
	handlers := make(map[string]func(context.Context, string, json.RawMessage) (string, error), len(tools))
	schemas := make(map[string]json.RawMessage, len(tools))
	for _, tool := range tools {
		if tool.Name == "submit_report" {
			return nil, nil, nil, nil, errors.New("agent: submit_report is reserved")
		}
		if !validToolName(tool.Name) || tool.Handler == nil {
			return nil, nil, nil, nil, fmt.Errorf("agent: tool %q needs a valid name and handler", tool.Name)
		}
		if _, exists := handlers[tool.Name]; exists {
			return nil, nil, nil, nil, fmt.Errorf("agent: duplicate tool name %q", tool.Name)
		}
		if err := validateSchemaDefinition(tool.Parameters); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("agent: invalid Parameters schema for tool %q: %w", tool.Name, err)
		}
		api = append(api, apiTool{Type: "function", Name: tool.Name, Description: tool.Description,
			Parameters: append(json.RawMessage(nil), tool.Parameters...), Strict: false})
		handlers[tool.Name] = tool.Handler
		schemas[tool.Name] = append(json.RawMessage(nil), tool.Parameters...)
	}
	api = append(api, apiTool{Type: "function", Name: "submit_report", Description: "Submit the final structured report for this role.",
		Parameters: append(json.RawMessage(nil), reportSchema...), Strict: false})
	return api, handlers, schemas, append(json.RawMessage(nil), reportSchema...), nil
}

func validToolName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

type functionCallItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (c *functionCallItem) validate() error {
	if c.ID == "" || c.CallID == "" || !validToolName(c.Name) {
		return errors.New("agent: function_call item is missing id, call_id, or valid name")
	}
	var encoded string
	if err := json.Unmarshal(c.Arguments, &encoded); err != nil {
		return fmt.Errorf("agent: function_call %q arguments are not a JSON string", c.Name)
	}
	if !json.Valid([]byte(encoded)) {
		return fmt.Errorf("agent: function_call %q arguments contain invalid JSON", c.Name)
	}
	c.Arguments = json.RawMessage(encoded)
	return nil
}

type functionOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}
