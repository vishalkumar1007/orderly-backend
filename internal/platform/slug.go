package platform

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/orderly/orderly-backend/pkg/response"
)

var reservedSlugs = map[string]struct{}{
	"admin": {}, "api": {}, "www": {}, "app": {}, "superadmin": {},
	"login": {}, "health": {}, "setup": {}, "static": {},
}

type slugCheckResult struct {
	Available bool   `json:"available"`
	Slug      string `json:"slug"`
	Reason    string `json:"reason,omitempty"`
}

func normalizeSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

func validateSlug(slug string) (slugCheckResult, bool) {
	slug = normalizeSlug(slug)
	if slug == "" {
		return slugCheckResult{Available: false, Slug: slug, Reason: "invalid_slug"}, false
	}
	if !slugRe.MatchString(slug) {
		return slugCheckResult{Available: false, Slug: slug, Reason: "invalid_slug"}, false
	}
	if _, ok := reservedSlugs[slug]; ok {
		return slugCheckResult{Available: false, Slug: slug, Reason: "slug_reserved"}, false
	}
	return slugCheckResult{Available: true, Slug: slug}, true
}

func (h *Handler) SlugAvailable(w http.ResponseWriter, r *http.Request) {
	slug := normalizeSlug(r.URL.Query().Get("slug"))
	res, ok := validateSlug(slug)
	if !ok {
		response.JSON(w, http.StatusOK, res)
		return
	}
	_, err := h.q.GetTenantBySlug(r.Context(), slug)
	if err == nil {
		response.JSON(w, http.StatusOK, slugCheckResult{Available: false, Slug: slug, Reason: "slug_taken"})
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		response.JSON(w, http.StatusOK, slugCheckResult{Available: true, Slug: slug})
		return
	}
	response.Error(w, http.StatusInternalServerError, "internal_error", "slug check failed")
}

func slugValidationError(slug string) (int, string, string) {
	res, ok := validateSlug(slug)
	if !ok {
		return http.StatusBadRequest, res.Reason, "invalid or reserved slug"
	}
	return 0, "", ""
}

func (h *Handler) EmailAvailable(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" || !strings.Contains(email, "@") {
		response.JSON(w, http.StatusOK, map[string]any{"available": false, "email": email, "reason": "invalid_email"})
		return
	}
	_, err := h.q.GetUserByEmail(r.Context(), email)
	if err == nil {
		response.JSON(w, http.StatusOK, map[string]any{"available": false, "email": email, "reason": "email_taken"})
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		response.JSON(w, http.StatusOK, map[string]any{"available": true, "email": email})
		return
	}
	response.Error(w, http.StatusInternalServerError, "internal_error", "email check failed")
}

