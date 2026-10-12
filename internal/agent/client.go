// Package agent implements a small, bounded client and function-calling loop
// for the OpenAI Responses API.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// Config contains the endpoint and limits used by both Client and Runner.
// BaseURL is the API root (for example https://api.openai.com/v1).
type Config struct {
	BaseURL         string
	Token           string
	Model           string
	HTTPClient      *http.Client
	Stream          bool
	MaxOutputTokens int
}

// Client sends requests to the Responses API. It does not fall back to the
// Chat Completions API.
type Client struct {
	baseURL string
	token   string
	model   string
	http    *http.Client
	stream  bool
}

// APIError describes an HTTP or Responses API failure.
type APIError struct {
	StatusCode int
	Status     string
	Message    string
	Type       string
	Code       string
	Param      string
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("Responses API: %s (HTTP %d): %s", e.Status, e.StatusCode, e.Message)
	}
	return "Responses API: " + e.Message
}

// NewClient validates configuration before any network request can be made.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("agent: BaseURL is required")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("agent: BaseURL must be an absolute http(s) URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("agent: Token is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("agent: Model is required")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, errors.New("agent: MaxOutputTokens cannot be negative")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("agent: BaseURL cannot contain a query or fragment")
	}
	base := strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/responses") {
		base += "/responses"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: base, token: cfg.Token, model: cfg.Model, http: hc, stream: cfg.Stream}, nil
}

// Doctor performs a small real Responses API request to confirm that the
// configured endpoint, credentials, and model work. NewClient rejects missing
// configuration, so this method cannot silently probe an unconfigured service.
func (c *Client) Doctor(ctx context.Context) error {
	if c == nil {
		return errors.New("agent: nil Client")
	}
	input, _ := json.Marshal("Reply with OK")
	_, err := c.createResponse(ctx, responseRequest{
		Model: c.model, Input: input, MaxOutputTokens: 1024,
		Store: false, Background: false,
	})
	return err
}

type responseRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             json.RawMessage `json:"input"`
	Tools             []apiTool       `json:"tools,omitempty"`
	MaxOutputTokens   int             `json:"max_output_tokens,omitempty"`
	Store             bool            `json:"store"`
	Background        bool            `json:"background"`
	Stream            bool            `json:"stream,omitempty"`
	Include           []string        `json:"include,omitempty"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
}

type apiTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type apiResponse struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Usage             Usage             `json:"usage"`
	IncompleteDetails json.RawMessage   `json:"incomplete_details"`
	Error             *apiErrorBody     `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

func (c *Client) createResponse(ctx context.Context, payload responseRequest) (apiResponse, error) {
	var zero apiResponse
	payload.Stream = c.stream
	payload.Include = []string{"reasoning.encrypted_content"}
	body, err := json.Marshal(payload)
	if err != nil {
		return zero, fmt.Errorf("agent: encode Responses request: %w", err)
	}
	endpoint := c.baseURL
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("agent: create Responses request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ai-workflow-mvp/0.1")
	if c.stream || payload.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("agent: Responses request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return zero, fmt.Errorf("agent: read Responses error body: %w", readErr)
		}
		return zero, decodeAPIError(resp.StatusCode, resp.Status, data)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var result apiResponse
	if mediaType == "text/event-stream" {
		result, err = readCompletedSSE(resp.Body)
	} else {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
		if readErr != nil {
			return zero, fmt.Errorf("agent: read Responses body: %w", readErr)
		}
		if len(data) > 16<<20 {
			return zero, errors.New("agent: Responses JSON exceeds 16 MiB")
		}
		err = json.Unmarshal(data, &result)
		if err != nil {
			err = fmt.Errorf("agent: decode Responses JSON: %w", err)
		}
	}
	if err != nil {
		return zero, err
	}
	if result.Error != nil {
		return zero, &APIError{Status: result.Status, Message: result.Error.Message, Type: result.Error.Type, Code: result.Error.Code, Param: result.Error.Param}
	}
	if result.Status != "completed" {
		detail := strings.TrimSpace(string(result.IncompleteDetails))
		if detail == "" || detail == "null" {
			detail = "no additional details"
		}
		return zero, fmt.Errorf("agent: Responses API status %q; %s", result.Status, detail)
	}
	return result, nil
}

func decodeAPIError(statusCode int, status string, data []byte) error {
	var envelope struct {
		Error *apiErrorBody `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
		e := envelope.Error
		return &APIError{StatusCode: statusCode, Status: status, Message: e.Message, Type: e.Type, Code: e.Code, Param: e.Param}
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return &APIError{StatusCode: statusCode, Status: status, Message: message}
}

// readCompletedSSE buffers the final response object. Partial output items are
// never exposed to Runner, which only executes tools after response.completed.
func readCompletedSSE(r io.Reader) (apiResponse, error) {
	var zero apiResponse
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 16<<20)
	var event string
	var data []string
	var completed *apiResponse
	flush := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		payload := strings.Join(data, "\n")
		var envelope struct {
			Type     string        `json:"type"`
			Response *apiResponse  `json:"response"`
			Error    *apiErrorBody `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			return fmt.Errorf("agent: decode Responses SSE event %q: %w", event, err)
		}
		kind := envelope.Type
		if kind == "" {
			kind = event
		}
		switch kind {
		case "response.completed":
			if envelope.Response == nil {
				return errors.New("agent: response.completed event has no response")
			}
			completed = envelope.Response
		case "response.failed", "error":
			if envelope.Error != nil {
				return &APIError{Message: envelope.Error.Message, Type: envelope.Error.Type, Code: envelope.Error.Code, Param: envelope.Error.Param}
			}
			if envelope.Response != nil && envelope.Response.Error != nil {
				return &APIError{Message: envelope.Response.Error.Message, Type: envelope.Response.Error.Type, Code: envelope.Response.Error.Code, Param: envelope.Response.Error.Param}
			}
			return fmt.Errorf("agent: Responses SSE event %q", kind)
		case "response.incomplete":
			if envelope.Response != nil {
				return fmt.Errorf("agent: Responses API status %q; %s", envelope.Response.Status, strings.TrimSpace(string(envelope.Response.IncompleteDetails)))
			}
			return errors.New("agent: Responses API returned an incomplete response")
		}
		data = nil
		event = ""
		return nil
	}
	for s.Scan() {
		line := strings.TrimSuffix(s.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return zero, err
			}
			if completed != nil {
				return *completed, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := s.Err(); err != nil {
		return zero, fmt.Errorf("agent: read Responses SSE stream: %w", err)
	}
	if err := flush(); err != nil {
		return zero, err
	}
	if completed == nil {
		return zero, fmt.Errorf("agent: Responses SSE stream ended before response.completed: %w", io.ErrUnexpectedEOF)
	}
	return *completed, nil
}
