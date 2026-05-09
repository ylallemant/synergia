package adminapi

import (
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/api"
)

// AdminBrandingAPI allows admins to update the worker branding CSS.
type AdminBrandingAPI struct {
	branding *api.BrandingAPI
}

func NewAdminBrandingAPI(b *api.BrandingAPI) *AdminBrandingAPI {
	return &AdminBrandingAPI{branding: b}
}

// AdminUpdateHandler updates the branding CSS.
// PUT /v1/admin/branding/css
func (a *AdminBrandingAPI) AdminUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	css := string(body)
	if err := a.branding.SetCSS(css); err != nil {
		log.Error().Err(err).Msg("failed to update branding CSS")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Info().Int("size_bytes", len(css)).Msg("branding CSS updated")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}
