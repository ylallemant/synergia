package adminapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/models"
	"github.com/ylallemant/synergia/internal/manager/store"
	"github.com/ylallemant/synergia/internal/protocol"
)

// ModelUpdatePusher is the interface for pushing model updates to connected workers.
type ModelUpdatePusher interface {
	PushModelUpdate(role, model, quantisation, filename, modelFileHash, llmHash string) error
}

// AdminRolesAPI manages role-model mappings.
type AdminRolesAPI struct {
	store      *store.Store
	gateway    ModelUpdatePusher
	modelStore models.Store
}

func NewAdminRolesAPI(s *store.Store) *AdminRolesAPI {
	return &AdminRolesAPI{store: s}
}

func (r *AdminRolesAPI) SetGateway(gw ModelUpdatePusher) { r.gateway = gw }
func (r *AdminRolesAPI) SetModelStore(ms models.Store)   { r.modelStore = ms }

// AdminRolesHandler handles GET/POST/PUT/DELETE on /v1/admin/roles.
func (r *AdminRolesAPI) AdminRolesHandler(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		roles, err := r.store.GetRoleModels()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(roles)

	case http.MethodPost, http.MethodPut:
		var req2 struct {
			Role          string `json:"role"`
			Model         string `json:"model"`
			Quantisation  string `json:"quantisation"`
			Filename      string `json:"filename"`
			ModelFileHash string `json:"model_file_hash"`
			MinVRAMMB     int    `json:"min_vram_mb"`
			Description   string `json:"description"`
		}
		if err := json.NewDecoder(req.Body).Decode(&req2); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req2.Role == "" || req2.Model == "" || req2.MinVRAMMB <= 0 {
			writeError(w, http.StatusBadRequest, "role, model, and min_vram_mb (>0) are required")
			return
		}
		if err := r.store.UpsertRoleModel(req2.Role, req2.Model, req2.Quantisation, req2.Filename, req2.ModelFileHash, req2.MinVRAMMB, req2.Description); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		log.Info().Str("role", req2.Role).Str("model", req2.Model).Str("filename", req2.Filename).Int("min_vram_mb", req2.MinVRAMMB).Msg("role-model mapping updated")

		if req2.ModelFileHash == "" && req2.Filename != "" && r.modelStore != nil {
			if hash, hashErr := r.modelStore.FileHash(context.Background(), req2.Filename); hashErr == nil {
				req2.ModelFileHash = hash
				_ = r.store.UpsertRoleModel(req2.Role, req2.Model, req2.Quantisation, req2.Filename, hash, req2.MinVRAMMB, req2.Description)
				log.Info().Str("role", req2.Role).Str("hash", hash[:16]+"...").Msg("computed model file hash from store")
			} else {
				log.Warn().Err(hashErr).Str("filename", req2.Filename).Msg("could not compute model file hash from store")
			}
		}

		if r.gateway != nil && req2.ModelFileHash != "" {
			llmHash := protocol.ComputeLLMHash(req2.Role, req2.ModelFileHash)
			if err := r.gateway.PushModelUpdate(req2.Role, req2.Model, req2.Quantisation, req2.Filename, req2.ModelFileHash, llmHash); err != nil {
				log.Warn().Err(err).Msg("failed to push model update to worker (may not be connected)")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case http.MethodDelete:
		role := req.URL.Query().Get("role")
		if role == "" {
			writeError(w, http.StatusBadRequest, "role query parameter is required")
			return
		}
		if err := r.store.DeleteRoleModel(role); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		log.Info().Str("role", role).Msg("role-model mapping deleted")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
