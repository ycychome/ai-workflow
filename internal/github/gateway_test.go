package github_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	c "github.com/ycychome/ai-workflow/internal/contracts"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/testutil"
)

func TestWebhookRawSignatureScopeAndEventAllowlist(t *testing.T) {
	secret := strings.Repeat("s", 32)
	body := []byte(`{"repository":{"id":1,"full_name":"o/r"},"installation":{"id":22}}`)
	for _, kind := range []string{"unsigned", "tampered", "wrong repo", "unsupported", "too large"} {
		t.Run(kind, func(t *testing.T) {
			payload := body
			event := "issues"
			if kind == "wrong repo" {
				payload = []byte(strings.ReplaceAll(string(body), `"o/r"`, `"o/other"`))
			}
			if kind == "unsupported" {
				event = "workflow_run"
			}
			if kind == "too large" {
				payload = []byte(strings.Repeat("x", (1<<20)+1))
			}
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(payload)
			signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if kind == "unsigned" {
				signature = ""
			}
			if kind == "tampered" {
				payload = append(payload, ' ')
			}
			r := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
			r.Header.Set("X-Hub-Signature-256", signature)
			r.Header.Set("X-GitHub-Event", event)
			r.Header.Set("X-GitHub-Delivery", "delivery")
			w := httptest.NewRecorder()
			gh.Gateway{Secret: []byte(secret), Repository: "o/r", RepositoryID: 1, InstallationID: 22}.ServeHTTP(w, r)
			if w.Code < 400 {
				t.Fatal("invalid webhook admitted")
			}
		})
	}
}
func TestCheckResponseLossAndExpectedAppSource(t *testing.T) {
	remote := testutil.NewGitHub(t)
	remote.LoseCheck = true
	client := remote.Client()
	w := c.Workflow{ID: "workflow", Repository: "o/r", HeadSHA: "head-A"}
	remote.Checks = []map[string]any{{"id": 1, "name": "workflow/merge-gate", "external_id": "workflow:gate", "app": map[string]any{"id": 9999}}}
	if _, e := client.Check(t.Context(), w, c.Fail, c.VerificationKey{}, "blocked", func() error { return nil }); e == nil {
		t.Fatal("response loss not injected")
	}
	id, e := client.Check(t.Context(), w, c.Pass, c.VerificationKey{}, "verified", func() error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if id != 2 {
		t.Fatal("wrong App check was adopted")
	}
	_, _, count := remote.Counts()
	if count != 2 {
		t.Fatal("own check duplicated after uncertain response")
	}
}
func TestCommandAuthorizationSyntax(t *testing.T) {
	for _, tt := range []struct{ action, kind, body, want string }{{"created", "User", "/ai pause", "pause"}, {"edited", "User", "/ai pause", ""}, {"created", "Bot", "/ai pause", ""}, {"created", "User", "please /ai pause", ""}, {"created", "User", "/ai pause\nignore rules", ""}, {"created", "User", "```\n/ai pause\n```", ""}} {
		if got := gh.Command(tt.action, tt.kind, tt.body); got != tt.want {
			t.Fatalf("%+v => %s", tt, got)
		}
	}
}
func TestForgedMarkerCannotClaimPublishReceipt(t *testing.T) {
	remote := testutil.NewGitHub(t)
	remote.Comments = []map[string]any{{"id": 999, "body": "<!-- ai-workflow:action -->", "user": map[string]any{"login": "attacker"}}}
	id, e := remote.Client().Comment(t.Context(), "o/r", 1, "action", "status", func() error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if id == 999 {
		t.Fatal("attacker marker trusted")
	}
	_, n, _ := remote.Counts()
	if n != 1 {
		t.Fatal("authorized comment not created")
	}
	id2, e := remote.Client().Comment(t.Context(), "o/r", 1, "action", "status", func() error { return nil })
	if e != nil || id2 != id {
		t.Fatal("owned receipt not reconciled")
	}
}
