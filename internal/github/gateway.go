package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/ycychome/ai-workflow/internal/persistence"
)

type Gateway struct {
	Store                        *persistence.Store
	Secret                       []byte
	Repository                   string
	RepositoryID, InstallationID int64
}

func (g Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if e != nil {
		http.Error(w, "payload too large", 413)
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	raw, e := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	mac := hmac.New(sha256.New, g.Secret)
	mac.Write(body)
	if len(g.Secret) < 32 || !strings.HasPrefix(sig, "sha256=") || e != nil || !hmac.Equal(raw, mac.Sum(nil)) {
		http.Error(w, "invalid signature", 401)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event != "issues" && event != "issue_comment" && event != "pull_request" && event != "push" {
		http.Error(w, "unsupported event", 400)
		return
	}
	id := r.Header.Get("X-GitHub-Delivery")
	if id == "" || len(id) > 128 {
		http.Error(w, "invalid delivery ID", 400)
		return
	}
	var envelope struct {
		Repository struct {
			ID   int64  `json:"id"`
			Name string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Repository.ID != g.RepositoryID || envelope.Repository.Name != g.Repository || envelope.Installation.ID != g.InstallationID {
		http.Error(w, "repository or installation not allowed", 403)
		return
	}
	if e = g.Store.Receive(r.Context(), id, event, body); e != nil {
		http.Error(w, "inbox unavailable", 503)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// Commands are exact, standalone, newly-created human comments.
func Command(action, kind, body string) string {
	if action != "created" || kind != "User" {
		return ""
	}
	switch strings.TrimSpace(body) {
	case "/ai pause":
		return "pause"
	case "/ai resume":
		return "resume"
	case "/ai cancel":
		return "cancel"
	case "/ai takeover":
		return "takeover"
	case "/ai status":
		return "status"
	}
	return ""
}
