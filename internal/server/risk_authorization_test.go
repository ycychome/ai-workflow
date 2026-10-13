package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthorizationCommandsRequireReviewedCandidateIdentity(t *testing.T) {
	for _, command := range []string{"waive F-1 key 2099-01-01T00:00:00Z 已检查风险", "authorize-risk key 2099-01-01T00:00:00Z 已审阅实际差异"} {
		h := &Handlers{}
		response := httptest.NewRecorder()
		h.command(response, httptest.NewRequest(http.MethodPost, "/api/workflows/workflow/commands", strings.NewReader(`{"command":"`+command+`"}`)))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "expected_generation") {
			t.Fatalf("authorization did not require reviewed identity: %d %s", response.Code, response.Body.String())
		}
	}
}
func TestAuthorizationCommandNamesAreExact(t *testing.T) {
	for _, command := range []string{"waive F key time reason", "authorize-risk key time reason"} {
		if !authorizationCommand(command) {
			t.Fatalf("valid authorization command rejected: %s", command)
		}
	}
	for _, command := range []string{"waive-anything", "authorize-risk-all", "resume", ""} {
		if authorizationCommand(command) {
			t.Fatalf("unknown authorization command accepted: %s", command)
		}
	}
}
