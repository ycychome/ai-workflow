package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListRecentOpenIssuesUsesBoundedIssueQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/55/access_tokens":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") == "" {
				t.Errorf("unexpected installation token request: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/repos/acme/pilot/issues":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "token installation-token" {
				t.Errorf("unexpected issues request: %s %s", r.Method, r.URL.Path)
			}
			want := map[string]string{"state": "open", "per_page": "100", "page": "1", "sort": "created", "direction": "desc"}
			for key, value := range want {
				if got := r.URL.Query().Get(key); got != value {
					t.Errorf("query %s = %q, want %q", key, got, value)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"number":7,"state":"open","title":"missed"},{"number":8,"state":"open","pull_request":{"url":"https://example.test/pr/8"}}]`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

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
	issues, err := client.ForInstallation(context.Background(), 55).ListRecentOpenIssues(context.Background(), "acme", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Number != 7 || issues[0].PullRequest != nil || issues[1].PullRequest == nil {
		t.Fatalf("unexpected issue results: %+v", issues)
	}
}
