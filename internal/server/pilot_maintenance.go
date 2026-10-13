package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ycychome/ai-workflow/internal/store"
)

const pilotRepository = "ycychome/ai-workflow-pilot"

// pilotMaintenance runs only on the authenticated dashboard surface. Tokens are
// caller-generated durable nonces; they are never written to an audit payload.
func (h *Handlers) pilotMaintenance(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.engine.Config().Repository(pilotRepository)
	if !ok || repo.ID <= 0 {
		writeError(w, 403, "未配置精确 pilot 仓库身份")
		return
	}
	operation := strings.TrimPrefix(r.URL.Path, "/api/pilot/maintenance/")
	if operation == "status" && r.Method == http.MethodGet {
		state, err := h.engine.Store.RepositoryMaintenanceStatus(r.Context(), repo.ID)
		if err != nil {
			writeError(w, 503, err.Error())
			return
		}
		writeJSON(w, 200, pilotMaintenanceView(state))
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Repository        string `json:"repository"`
		RepositoryID      int64  `json:"repository_id"`
		ExpectedEpoch     string `json:"expected_epoch"`
		NewEpoch          string `json:"new_epoch"`
		PlanDigest        string `json:"plan_sha256"`
		Token             string `json:"maintenance_token"`
		KeepIntakeStopped bool   `json:"keep_intake_stopped"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, 400, "维护请求格式无效")
		return
	}
	if req.Repository != pilotRepository || req.RepositoryID != repo.ID || len(req.Token) < 32 || req.ExpectedEpoch == "" || req.NewEpoch == "" || req.PlanDigest == "" {
		writeError(w, 403, "维护请求缺少精确仓库、代次、计划或持久令牌")
		return
	}
	plan := store.RepositoryMaintenance{RepositoryID: req.RepositoryID, Token: req.Token, OldEpoch: req.ExpectedEpoch, NewEpoch: req.NewEpoch, PlanDigest: req.PlanDigest}
	var err error
	switch operation {
	case "acquire":
		var state store.RepositoryMaintenanceStatus
		state, err = h.engine.Store.AcquireRepositoryMaintenance(r.Context(), plan)
		if err == nil {
			view := pilotMaintenanceView(state)
			view["maintenance_token"] = req.Token
			writeJSON(w, 200, view)
			return
		}
	case "retire":
		err = h.engine.Store.RetireRepositoryMaintenance(r.Context(), plan)
		if err == nil {
			err = h.engine.PersistRetiredRepositoryEpoch(r.Context(), req.RepositoryID, req.ExpectedEpoch, req.NewEpoch, h.persistConfig)
		}
	case "resume":
		state, e := h.engine.Store.RepositoryMaintenanceStatus(r.Context(), repo.ID)
		if e != nil {
			err = e
			break
		}
		if state.Held || !state.Retired || repo.EffectiveEpoch() != state.Epoch {
			writeError(w, 409, "恢复前须完成保护核验、释放维护锁并持久同步仓库配置代次")
			return
		}
		err = h.engine.Store.ReleaseRepositoryMaintenance(r.Context(), plan, false)
	case "release":
		if !req.KeepIntakeStopped {
			writeError(w, 403, "重置维护接口仅允许保持停收；恢复运行须独立核对配置和代次")
			return
		}
		err = h.engine.Store.ReleaseRepositoryMaintenance(r.Context(), plan, true)
	default:
		writeError(w, 404, "未知维护操作")
		return
	}
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	state, err := h.engine.Store.RepositoryMaintenanceStatus(r.Context(), repo.ID)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	writeJSON(w, 200, pilotMaintenanceView(state))
}
func pilotMaintenanceView(s store.RepositoryMaintenanceStatus) map[string]any {
	return map[string]any{"repository": pilotRepository, "repository_id": s.RepositoryID, "repository_epoch": s.Epoch, "held": s.Held, "intake_stopped": s.IntakeStopped, "retired": s.Retired, "active_agents": s.ActiveAgents, "stopping_agents": s.StoppingAgents, "active_workflow_leases": s.LiveWorkflowLeases, "inflight_outbox": s.RunningOutbox + s.UnknownOutbox, "running_outbox": s.RunningOutbox, "unknown_outbox": s.UnknownOutbox, "maintenance_lock_capable": true}
}
