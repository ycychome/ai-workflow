// Package issuedesk 是用于开发代理评估的多租户工单服务。
package issuedesk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Actor struct {
	ID       int64  `json:"id"`
	TenantID int64  `json:"tenant_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}
type Ticket struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Status     string    `json:"status"`
	CreatorID  int64     `json:"creator_id"`
	AssigneeID *int64    `json:"assignee_id"`
	Version    int64     `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
type Comment struct {
	ID        int64     `json:"id"`
	AuthorID  int64     `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type problem struct {
	status        int
	code, message string
}

func (e *problem) Error() string                  { return e.message }
func fail(status int, code, message string) error { return &problem{status, code, message} }
func tokenHash(s string) string                   { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func jsonBytes(v any) []byte                      { b, _ := json.Marshal(v); return b }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func etag(v int64) string { return fmt.Sprintf(`"%d"`, v) }
func allowedTransition(role, from, to string) bool {
	if from == to {
		return true
	}
	if role == "requester" {
		return from == "open" && to == "cancelled"
	}
	allowed := map[string]map[string]bool{"open": {"in_progress": true, "cancelled": true}, "in_progress": {"resolved": true, "cancelled": true}, "resolved": {"closed": true, "in_progress": true}}
	return allowed[from][to]
}
