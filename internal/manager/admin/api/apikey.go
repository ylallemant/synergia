package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ylallemant/synergia/internal/manager/store"
)

const settingAPIKey = "api_key"

type AdminAPIKeyAPI struct {
	store     *store.Store
	onUpdated func(string)
}

func NewAdminAPIKeyAPI(s *store.Store, onUpdated func(string)) *AdminAPIKeyAPI {
	return &AdminAPIKeyAPI{store: s, onUpdated: onUpdated}
}

func (a *AdminAPIKeyAPI) AdminAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		key, _ := a.store.GetSetting(settingAPIKey)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"key": key})

	case http.MethodPost:
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		body.Key = strings.TrimSpace(body.Key)
		if body.Key == "" {
			http.Error(w, "key must not be empty", http.StatusBadRequest)
			return
		}
		if err := a.store.SetSetting(settingAPIKey, body.Key); err != nil {
			http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if a.onUpdated != nil {
			a.onUpdated(body.Key)
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
