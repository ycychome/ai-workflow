package server

import (
	"net/http"
	"strings"

	"github.com/ycychome/ai-workflow/internal/orchestration"
)

// riskPreview is read-only and runs behind dashboardAPI's session check.
// The eventual command recomputes the real diff and verifies its exact key.
func (h *Handlers) riskPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/workflows/"), "/risk-preview")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, "工作流 ID 无效")
		return
	}
	workflow, err := h.engine.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "未找到工作流")
		return
	}
	changes, err := h.engine.CurrentRiskChanges(r.Context(), workflow)
	if err != nil {
		writeError(w, http.StatusConflict, "无法读取当前冻结候选的实际差异："+err.Error())
		return
	}
	risks, err := orchestration.ClassifyRiskChanges(changes)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow_id": id, "generation": workflow.Generation, "head_sha": workflow.HeadSHA, "verification_key": workflow.Review.Key, "changes": changes, "risks": risks, "actor": "local-dashboard"})
}
func authorizationCommand(command string) bool {
	fields := strings.Fields(command)
	return len(fields) > 0 && (fields[0] == "waive" || fields[0] == "authorize-risk")
}
