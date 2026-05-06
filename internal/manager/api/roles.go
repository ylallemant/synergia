package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/models"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// ModelUpdatePusher is an interface for pushing model updates to connected workers.
type ModelUpdatePusher interface {
	PushModelUpdate(role, model, quantisation, filename, modelFileHash, llmHash string) error
}

// RolesAPI handles role-model mapping queries and eligibility checks.
type RolesAPI struct {
	workerKey  string
	apiKey     string
	store      *store.Store
	testSetup  bool
	gateway    ModelUpdatePusher
	modelStore models.Store
}

func NewRolesAPI(apiKey, workerKey string, s *store.Store, testSetup bool) *RolesAPI {
	return &RolesAPI{
		apiKey:    apiKey,
		workerKey: workerKey,
		store:     s,
		testSetup: testSetup,
	}
}

// SetGateway sets the gateway for pushing model updates to workers.
func (r *RolesAPI) SetGateway(gw ModelUpdatePusher) {
	r.gateway = gw
}

// SetModelStore sets the model store for computing file hashes.
func (r *RolesAPI) SetModelStore(ms models.Store) {
	r.modelStore = ms
}

// RoleInfo is returned to clients showing available roles and eligibility.
type RoleInfo struct {
	Role         string `json:"role"`
	Model        string `json:"model"`
	Quantisation string `json:"quantisation"`
	MinVRAMMB    int    `json:"min_vram_mb"`
	Description  string `json:"description"`
	Eligible     bool   `json:"eligible"`
}

// RolesHandler handles GET /v1/roles?fingerprint=<fp>
// Returns all roles with eligibility computed from the worker's hardware.
// If no fingerprint is provided, returns roles without eligibility (all marked eligible=false).
func (r *RolesAPI) RolesHandler(w http.ResponseWriter, req *http.Request) {
	if !r.authenticate(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	roles, err := r.store.GetRoleModels()
	if err != nil {
		log.Error().Err(err).Msg("failed to query role models")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	// Determine worker's VRAM from consent record (if fingerprint provided)
	fingerprint := req.URL.Query().Get("fingerprint")
	var workerVRAM int
	if fingerprint != "" {
		consent, err := r.store.GetConsent(fingerprint)
		if err == nil && consent != nil {
			workerVRAM = consent.HwVRAMMB
		}
	}

	if r.testSetup {
		log.Debug().
			Int("worker_vram_mb", workerVRAM).
			Int("roles_count", len(roles)).
			Str("fingerprint", fingerprint).
			Msg("test-setup: serving roles with faked VRAM thresholds (512 MB) — real thresholds are 4-20 GB")
	}

	result := make([]RoleInfo, 0, len(roles))
	for _, rm := range roles {
		info := RoleInfo{
			Role:         rm.Role,
			Model:        rm.LLMModel,
			Quantisation: rm.Quantisation,
			MinVRAMMB:    rm.MinVRAMMB,
			Description:  rm.Description,
			Eligible:     workerVRAM >= rm.MinVRAMMB,
		}
		result = append(result, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// AdminRolesHandler handles GET/POST/PUT on /v1/admin/roles for managing role-model mappings.
func (r *RolesAPI) AdminRolesHandler(w http.ResponseWriter, req *http.Request) {
	if !r.authenticateAdmin(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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

		// If no file hash provided but filename exists, try to compute from model store
		if req2.ModelFileHash == "" && req2.Filename != "" && r.modelStore != nil {
			if hash, hashErr := r.modelStore.FileHash(context.Background(), req2.Filename); hashErr == nil {
				req2.ModelFileHash = hash
				// Persist the computed hash
				_ = r.store.UpsertRoleModel(req2.Role, req2.Model, req2.Quantisation, req2.Filename, hash, req2.MinVRAMMB, req2.Description)
				log.Info().Str("role", req2.Role).Str("hash", hash[:16]+"...").Msg("computed model file hash from store")
			} else {
				log.Warn().Err(hashErr).Str("filename", req2.Filename).Msg("could not compute model file hash from store")
			}
		}

		// Push model_update to connected workers so they can switch to the new configuration
		if r.gateway != nil && req2.ModelFileHash != "" {
			llmHash := store.ComputeLLMHash(req2.Role, req2.ModelFileHash)
			if err := r.gateway.PushModelUpdate(req2.Role, req2.Model, req2.Quantisation, req2.Filename, req2.ModelFileHash, llmHash); err != nil {
				log.Warn().Err(err).Msg("failed to push model update to worker (may not be connected)")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *RolesAPI) authenticate(req *http.Request) bool {
	auth := req.Header.Get("Authorization")
	if auth == "Bearer "+r.workerKey {
		return true
	}
	if auth == "Bearer "+r.apiKey {
		return true
	}
	return false
}

func (r *RolesAPI) authenticateAdmin(req *http.Request) bool {
	auth := req.Header.Get("Authorization")
	return auth == "Bearer "+r.apiKey
}
