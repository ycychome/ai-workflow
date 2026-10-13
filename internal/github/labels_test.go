package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestSyncManagedLabelsCreatesAddsAndRemovesOnlyManagedLabels(t *testing.T) {
	var added []string
	deleted := []string{}
	labelLookups := 0
	issuePages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/55/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/repos/acme/pilot/issues/7/labels":
			if r.Method == http.MethodGet {
				page := r.URL.Query().Get("page")
				if r.URL.Query().Get("per_page") != "100" {
					t.Errorf("unexpected issue label list query: %s", r.URL.RawQuery)
				}
				issuePages++
				if page == "1" {
					labels := []issueLabel{{Name: "user-label"}, {Name: "bot:state-old"}}
					for n := 0; n < 98; n++ {
						labels = append(labels, issueLabel{Name: "custom-label"})
					}
					body, _ := json.Marshal(labels)
					_, _ = w.Write(body)
				} else if page == "2" {
					_, _ = w.Write([]byte(`[]`))
				} else {
					t.Errorf("unexpected issue label page %q", page)
				}
			} else if r.Method == http.MethodPost {
				var body struct {
					Labels []string `json:"labels"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				added = body.Labels
				_, _ = w.Write([]byte(`[]`))
			} else {
				t.Errorf("unexpected issue labels method: %s", r.Method)
			}
		case "/repos/acme/pilot/labels/bot:state-ready":
			if r.Method != http.MethodGet {
				t.Errorf("expected label lookup, got %s", r.Method)
			}
			labelLookups++
			if labelLookups == 1 {
				http.NotFound(w, r)
			} else {
				_, _ = w.Write([]byte(`{"name":"bot:state-ready","color":"2f6feb"}`))
			}
		case "/repos/acme/pilot/labels":
			if r.Method != http.MethodPost {
				t.Errorf("expected label create, got %s", r.Method)
			}
			var label Label
			if err := json.NewDecoder(r.Body).Decode(&label); err != nil {
				t.Fatal(err)
			}
			if label.Name != "bot:state-ready" || label.Color != "2f6feb" || label.Description != "Ready" {
				t.Errorf("unexpected created label: %+v", label)
			}
			// Simulate a concurrent creator winning the race after our GET.
			http.Error(w, `{"message":"already_exists"}`, http.StatusUnprocessableEntity)
		case "/repos/acme/pilot/issues/7/labels/bot:state-ready":
			if r.Method != http.MethodGet {
				t.Errorf("expected race confirmation GET, got %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"name":"bot:state-ready","color":"2f6feb"}`))
		case "/repos/acme/pilot/issues/7/labels/bot:state-old":
			if r.Method != http.MethodDelete {
				t.Errorf("expected delete stale managed label, got %s", r.Method)
			}
			deleted = append(deleted, "bot:state-old")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newLabelTestClient(t, server)
	err := client.ForInstallation(context.Background(), 55).SyncManagedLabels(context.Background(), "acme", "pilot", 7, "bot:state-", []Label{{Name: "bot:state-ready", Color: "2f6feb", Description: "Ready"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"bot:state-ready"}) {
		t.Fatalf("added labels = %v", added)
	}
	if !reflect.DeepEqual(deleted, []string{"bot:state-old"}) {
		t.Fatalf("deleted labels = %v", deleted)
	}
	if issuePages != 2 {
		t.Fatalf("issue labels pages = %d, want 2", issuePages)
	}
}

func TestSyncManagedLabelsRejectsEmptyPrefixBeforeNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newLabelTestClient(t, server)
	err := client.ForInstallation(context.Background(), 55).SyncManagedLabels(context.Background(), "acme", "pilot", 7, " ", nil)
	if err == nil {
		t.Fatal("expected empty prefix validation error")
	}
}

func newLabelTestClient(t *testing.T, server *httptest.Server) *Client {
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
