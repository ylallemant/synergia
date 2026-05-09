package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// RolesAPI handles role-model mapping queries for workers.
type RolesAPI struct {
	workerKey string
	apiKey    string
	store     *store.Store
	testSetup bool
}

func NewRolesAPI(apiKey, workerKey string, s *store.Store, testSetup bool) *RolesAPI {
	return &RolesAPI{
		apiKey:    apiKey,
		workerKey: workerKey,
		store:     s,
		testSetup: testSetup,
	}
}

// RoleInfo is returned to workers showing available roles and eligibility.
type RoleInfo struct {
	Role         string `json:"role"`
	Model        string `json:"model"`
	Quantisation string `json:"quantisation"`
	MinVRAMMB    int    `json:"min_vram_mb"`
	Description  string `json:"description"`
	Eligible     bool   `json:"eligible"`
}

// RolesHandler handles GET /v1/roles?fingerprint=<fp>
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
			Eligible:     rm.Role == "tester" || workerVRAM >= rm.MinVRAMMB,
		}
		result = append(result, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
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
