package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// VerifyWebhook validates X-Hub-Signature-256 against the original request bytes.
func VerifyWebhook(secret string, raw []byte, signature string) error {
	if secret == "" {
		return errors.New("github: webhook secret is required")
	}
	if !strings.HasPrefix(signature, "sha256=") {
		return errors.New("github: missing sha256 webhook signature")
	}
	want, e := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if e != nil {
		return errors.New("github: malformed webhook signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("github: invalid webhook signature")
	}
	return nil
}

type WebhookRepository struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}
type WebhookInstallation struct {
	ID      int64   `json:"id"`
	Account Account `json:"account"`
}
type WebhookComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	User    User   `json:"user"`
	HTMLURL string `json:"html_url"`
}

// WebhookEvent is a deliberately small, stable projection of common GitHub webhook fields.
type WebhookEvent struct {
	Action       string               `json:"action"`
	ActionID     string               `json:"action_id,omitempty"`
	Issue        *Issue               `json:"issue,omitempty"`
	PullRequest  *PullRequest         `json:"pull_request,omitempty"`
	Comment      *WebhookComment      `json:"comment,omitempty"`
	Repository   *WebhookRepository   `json:"repository,omitempty"`
	Installation *WebhookInstallation `json:"installation,omitempty"`
	Sender       *User                `json:"sender,omitempty"`
	Ref          string               `json:"ref,omitempty"`
	Before       string               `json:"before,omitempty"`
	After        string               `json:"after,omitempty"`
	CheckRun     *CheckRun            `json:"check_run,omitempty"`
	CheckSuite   *struct {
		ID         int64  `json:"id"`
		HeadSHA    string `json:"head_sha"`
		Conclusion string `json:"conclusion"`
		Status     string `json:"status"`
	} `json:"check_suite,omitempty"`
}

func ParseWebhook(raw []byte) (WebhookEvent, error) {
	var e WebhookEvent
	if len(raw) == 0 {
		return e, errors.New("github: empty webhook payload")
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("github: invalid webhook JSON: %w", err)
	}
	return e, nil
}

func (c *Client) VerifyAndParseWebhook(secret, signature string, raw []byte) (WebhookEvent, error) {
	if err := VerifyWebhook(secret, raw, signature); err != nil {
		return WebhookEvent{}, err
	}
	return ParseWebhook(raw)
}
