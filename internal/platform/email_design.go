package platform

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// brandingWrite is the editable part of a sender's email branding.
type brandingWrite struct {
	ShowLogo   bool   `json:"show_logo"`
	LogoURL    string `json:"logo_url"`
	BrandColor string `json:"brand_color"`
	FooterText string `json:"footer_text"`
	Theme      string `json:"theme"`
	PageColor  string `json:"page_color"`
	CardColor  string `json:"card_color"`
	Layout     string `json:"layout"`
}

var brandHex = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func (b *brandingWrite) validate() string {
	b.LogoURL, b.FooterText = strings.TrimSpace(b.LogoURL), strings.TrimSpace(b.FooterText)
	b.BrandColor = strings.ToUpper(strings.TrimSpace(b.BrandColor))
	if b.Theme == "" {
		b.Theme = "classic"
	}
	if b.Layout == "" {
		b.Layout = "classic"
	}
	if !slices.Contains(notify.Layouts(), b.Layout) {
		return "layout must be one of " + strings.Join(notify.Layouts(), ", ")
	}
	if !slices.Contains(notify.ThemeNames(), b.Theme) {
		return "theme must be one of " + strings.Join(notify.ThemeNames(), ", ")
	}
	b.PageColor, b.CardColor = strings.ToUpper(strings.TrimSpace(b.PageColor)), strings.ToUpper(strings.TrimSpace(b.CardColor))
	if b.Theme == "custom" && (!brandHex.MatchString(b.PageColor) || !brandHex.MatchString(b.CardColor)) {
		return "a custom theme needs a background and a card colour like #F5F6F8"
	}
	if b.Theme != "custom" {
		b.PageColor, b.CardColor = "", ""
	}
	switch {
	case !brandHex.MatchString(b.BrandColor):
		return "brand_color must be a colour like #0F766E"
	case b.LogoURL != "" && !strings.HasPrefix(b.LogoURL, "https://") && !strings.HasPrefix(b.LogoURL, "http://"):
		return "logo_url must be a web address (http or https): mail clients cannot show embedded images"
	case strings.Contains(b.LogoURL, "X-Amz-Signature="):
		// A presigned link stops working in minutes, long before the email is read.
		return "this logo link expires soon: set a Public URL on the storage provider, or use a permanent image link"
	case len(b.LogoURL) > 2048:
		return "logo_url is too long"
	case len([]rune(b.FooterText)) > 300:
		return "footer_text must be 300 characters or fewer"
	}
	return ""
}

/* ------------------------------------------------------------------ *
 * Branding
 * ------------------------------------------------------------------ */

func (h *Handler) GetPlatformEmailBranding(w http.ResponseWriter, r *http.Request) {
	row, err := h.q.GetEmailBranding(r.Context(), pgutil.NullUUID(nil))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load email branding")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"branding":    brandingWrite{ShowLogo: row.ShowLogo, LogoURL: row.LogoUrl, BrandColor: row.BrandColor, FooterText: row.FooterText, Theme: row.Theme, PageColor: row.PageColor, CardColor: row.CardColor, Layout: row.Layout},
		"sender_name": "Orderly",
	})
}

func (h *Handler) PutPlatformEmailBranding(w http.ResponseWriter, r *http.Request) {
	h.putBranding(w, r, nil)
}

func (h *Handler) GetTenantEmailBranding(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	identity, err := h.q.GetTenantEmailIdentity(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load email branding")
		return
	}
	out := brandingWrite{ShowLogo: true, BrandColor: "#0F766E", Theme: "classic", Layout: "classic"}
	if p, err := h.q.GetEmailBranding(r.Context(), pgutil.NullUUID(nil)); err == nil {
		out.BrandColor, out.Theme, out.Layout = p.BrandColor, p.Theme, p.Layout
	}
	if row, err := h.q.GetEmailBranding(r.Context(), pgutil.NullUUID(&tenantID)); err == nil {
		out = brandingWrite{ShowLogo: row.ShowLogo, LogoURL: row.LogoUrl, BrandColor: row.BrandColor, FooterText: row.FooterText, Theme: row.Theme, PageColor: row.PageColor, CardColor: row.CardColor, Layout: row.Layout}
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"branding":            out,
		"sender_name":         identity.DisplayName,
		"storefront_logo_url": identity.LogoUrl,
	})
}

func (h *Handler) PutTenantEmailBranding(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	if !h.requireFeature(w, r, tenantID, entitlement.FeatureNotifyTemplates) {
		return
	}
	h.putBranding(w, r, &tenantID)
}

func (h *Handler) putBranding(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	var req brandingWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	if msg := req.validate(); msg != "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	n, err := h.q.UpdateEmailBranding(r.Context(), sqlc.UpdateEmailBrandingParams{
		TenantID: pgutil.NullUUID(tenantID), ShowLogo: req.ShowLogo, LogoUrl: req.LogoURL,
		BrandColor: req.BrandColor, FooterText: req.FooterText, Theme: req.Theme,
		PageColor: req.PageColor, CardColor: req.CardColor, Layout: req.Layout,
	})
	if err == nil && n == 0 && tenantID != nil {
		err = h.q.InsertTenantEmailBranding(r.Context(), sqlc.InsertTenantEmailBrandingParams{
			TenantID: pgutil.UUID(*tenantID), ShowLogo: req.ShowLogo, LogoUrl: req.LogoURL,
			BrandColor: req.BrandColor, FooterText: req.FooterText, Theme: req.Theme,
			PageColor: req.PageColor, CardColor: req.CardColor, Layout: req.Layout,
		})
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save email branding")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"branding": req})
}

/* ------------------------------------------------------------------ *
 * Preview
 * ------------------------------------------------------------------ */

type previewRequest struct {
	EventCode string              `json:"event_code"`
	Content   notify.EmailContent `json:"content"`
	// Branding, when given, is the unsaved branding draft to preview with.
	Branding *brandingWrite `json:"branding,omitempty"`
	// Raw keeps the {{variables}} in place: the layout as a starting point
	// for writing the email in HTML.
	Raw bool `json:"raw,omitempty"`
}

func (h *Handler) PreviewPlatformEmail(w http.ResponseWriter, r *http.Request) {
	h.previewEmail(w, r, nil)
}

func (h *Handler) PreviewTenantEmail(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	h.previewEmail(w, r, &tenantID)
}

// previewEmail renders a draft exactly as it would be sent: same layout code,
// sample values for the variables: so the editor never drifts from the inbox.
func (h *Handler) previewEmail(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	var req previewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	branding := notify.ResolveBranding(r.Context(), h.q, tenantID)
	if b := req.Branding; b != nil {
		branding.ShowLogo, branding.BrandColor, branding.Theme = b.ShowLogo, b.BrandColor, b.Theme
		branding.PageColor, branding.CardColor, branding.Layout = b.PageColor, b.CardColor, b.Layout
		branding.LogoURL = strings.TrimSpace(b.LogoURL)
		if tenantID != nil {
			if branding.LogoURL == "" {
				if id, err := h.q.GetTenantEmailIdentity(r.Context(), pgutil.UUID(*tenantID)); err == nil {
					branding.LogoURL = id.LogoUrl
				}
			}
			if strings.TrimSpace(b.FooterText) == "" {
				b.FooterText = "You are receiving this email from " + branding.SenderName + "."
			}
		}
		branding.FooterText = b.FooterText
	}
	subject, text, html := notify.PreviewEmail(r.Context(), h.q, req.EventCode, req.Content, branding, req.Raw)
	out := map[string]any{"subject": subject, "text": text, "html": html}
	if req.Raw {
		// The content block alone, for writing HTML inside the branded layout.
		out["content"] = notify.RawContent(req.Content, branding)
	}
	response.JSON(w, http.StatusOK, out)
}

/* ------------------------------------------------------------------ *
 * Email logos
 * ------------------------------------------------------------------ */

// Email-safe image types only: Gmail and Outlook do not render SVG or WebP.
var emailLogoTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif"}

var emailAssetName = regexp.MustCompile(`^[0-9a-f-]{36}\.(png|jpg|gif)$`)

func (h *Handler) UploadPlatformEmailLogo(w http.ResponseWriter, r *http.Request) {
	h.uploadEmailLogo(w, r, nil)
}

func (h *Handler) UploadTenantEmailLogo(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	if !h.requireFeature(w, r, tenantID, entitlement.FeatureNotifyTemplates) {
		return
	}
	h.uploadEmailLogo(w, r, &tenantID)
}

// uploadEmailLogo stores a logo and returns a permanent address for it. A
// storage upload's own link is presigned and dies in hours, but an email is
// read days later, so unless the bucket is public the address returned is
// Orderly's own redirect, which signs a fresh link on every open.
func (h *Handler) uploadEmailLogo(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	if h.configs == nil || h.configs.StorageService() == nil {
		response.Error(w, http.StatusServiceUnavailable, "storage_not_configured", "Storage is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "The logo must be under 2 MB")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "No file provided")
		return
	}
	defer file.Close()
	contentType := header.Header.Get("Content-Type")
	ext, ok := emailLogoTypes[contentType]
	data, readErr := io.ReadAll(file)
	if !ok || readErr != nil || !isValidAdminImage(data, contentType) && contentType != "image/gif" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Use a PNG, JPG or GIF: mail apps do not show SVG or WebP")
		return
	}

	name := uuid.NewString() + ext
	key := "email-assets/" + name
	storage := h.configs.StorageService()
	req := configsvc.PutRequest{Key: key, Body: data, ContentType: contentType}
	var obj configsvc.StoredObject
	scope := "p"
	if tenantID == nil {
		obj, err = storage.PutPlatform(r.Context(), req)
	} else {
		obj, err = storage.Put(r.Context(), *tenantID, req)
		scope = tenantID.String()
	}
	if err != nil {
		response.Error(w, http.StatusBadGateway, "upload_failed", configsvc.SafeErrorMessage(err))
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"url": h.assetURL(obj, scope, key)})
}

// EmailAsset serves an email logo by redirecting to a freshly signed storage
// link. Public by necessity (mail clients fetch images anonymously) and
// narrow by design: only names an email-logo upload can produce.
func (h *Handler) EmailAsset(w http.ResponseWriter, r *http.Request) {
	scope, name := chi.URLParam(r, "scope"), chi.URLParam(r, "name")
	if !emailAssetName.MatchString(name) || h.configs == nil || h.configs.StorageService() == nil {
		http.NotFound(w, r)
		return
	}
	key := "email-assets/" + name
	storage := h.configs.StorageService()
	var url string
	var err error
	if scope == "p" {
		url, err = storage.PresignGetPlatform(r.Context(), key, time.Hour)
	} else if tid, perr := uuid.Parse(scope); perr == nil {
		url, err = storage.PresignGet(r.Context(), tid, key, time.Hour)
	} else {
		http.NotFound(w, r)
		return
	}
	if err != nil || url == "" {
		http.NotFound(w, r)
		return
	}
	// Shorter than the signature, so a cached redirect never points at a dead link.
	w.Header().Set("Cache-Control", "public, max-age=1800")
	http.Redirect(w, r, url, http.StatusFound)
}
