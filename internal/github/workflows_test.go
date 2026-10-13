package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetCommitInfoParsesGitDatabaseCommitObject(t *testing.T) {
	const commitSHA = "7638417db6d59f3c431d3e1f261cc637155684cd"
	const treeSHA = "827efc6d56897b048c772eb4087f854f46256132"
	const parentSHA = "7d1b31e74ee336d15cbd21741bc88a537ed063a0"
	server := newCommitInfoServer(t, `{
		"sha":"`+commitSHA+`",
		"message":"my commit message",
		"tree":{"url":"https://api.github.com/repos/acme/pilot/git/trees/`+treeSHA+`","sha":"`+treeSHA+`"},
		"parents":[{"url":"https://api.github.com/repos/acme/pilot/git/commits/`+parentSHA+`","sha":"`+parentSHA+`"}],
		"verification":{"verified":false,"reason":"unsigned","signature":null,"payload":null,"verified_at":null}
	}`)
	defer server.Close()

	client := newCommitInfoTestClient(t, server)
	got, err := client.ForInstallation(context.Background(), 9).GetCommitInfo(context.Background(), "acme", "pilot", commitSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != commitSHA || got.TreeSHA != treeSHA || got.Message != "my commit message" || len(got.Parents) != 1 || got.Parents[0] != parentSHA {
		t.Fatalf("unexpected commit info: %+v", got)
	}
}

func TestGetCommitInfoAllowsInitialCommitWithoutParents(t *testing.T) {
	const commitSHA = "7638417db6d59f3c431d3e1f261cc637155684cd"
	server := newCommitInfoServer(t, `{"sha":"`+commitSHA+`","message":"initial commit","tree":{"sha":"tree-sha"},"parents":[]}`)
	defer server.Close()
	client := newCommitInfoTestClient(t, server)
	got, err := client.ForInstallation(context.Background(), 9).GetCommitInfo(context.Background(), "acme", "pilot", commitSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != commitSHA || got.Message != "initial commit" || got.TreeSHA != "tree-sha" || len(got.Parents) != 0 {
		t.Fatalf("unexpected initial commit info: %+v", got)
	}
}

func TestGetCommitInfoRejectsIncompleteOrMismatchedCommitObjects(t *testing.T) {
	const commitSHA = "7638417db6d59f3c431d3e1f261cc637155684cd"
	tests := []struct {
		name string
		body string
	}{
		{name: "missing sha", body: `{"message":"message","tree":{"sha":"tree"},"parents":[]}`},
		{name: "mismatched sha", body: `{"sha":"other-sha","message":"message","tree":{"sha":"tree"},"parents":[]}`},
		{name: "missing tree", body: `{"sha":"` + commitSHA + `","message":"message","tree":{},"parents":[]}`},
		{name: "missing message", body: `{"sha":"` + commitSHA + `","tree":{"sha":"tree"},"parents":[]}`},
		{name: "empty parent sha", body: `{"sha":"` + commitSHA + `","message":"message","tree":{"sha":"tree"},"parents":[{"sha":""}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newCommitInfoServer(t, tt.body)
			defer server.Close()
			client := newCommitInfoTestClient(t, server)
			_, err := client.ForInstallation(context.Background(), 9).GetCommitInfo(context.Background(), "acme", "pilot", commitSHA)
			if err == nil {
				t.Fatal("expected malformed commit object to fail")
			}
		})
	}
}

func TestGetCommitInfoRequiresRequestIdentity(t *testing.T) {
	server := newCommitInfoServer(t, `{}`)
	defer server.Close()
	client := newCommitInfoTestClient(t, server)
	_, err := client.ForInstallation(context.Background(), 9).GetCommitInfo(context.Background(), "acme", "pilot", " ")
	if err == nil || !strings.Contains(err.Error(), "commit SHA are required") {
		t.Fatalf("expected SHA validation error, got %v", err)
	}
}

func newCommitInfoServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/9/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/repos/acme/pilot/git/commits/7638417db6d59f3c431d3e1f261cc637155684cd":
			if r.Method != http.MethodGet {
				t.Errorf("expected GET commit object, got %s", r.Method)
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func newCommitInfoTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	client, err := New(Config{
		AppID: "1", PrivateKeyPEM: privateKey, BaseURL: server.URL,
		HTTPClient: server.Client(), Now: func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
