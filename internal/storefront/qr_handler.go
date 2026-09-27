package storefront

import (
	"net/http"

	"github.com/orderly/orderly-backend/pkg/response"
)

// QRCode returns the tenant's storefront address as a QR code plus the plain
// URL, so the admin screen can show the code, copy the link and preview the
// store from one place.
func (a *AdminHandler) QRCode(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	url := a.base(sf.Slug)
	// A tenant that has customised its brand colour gets a matching code,
	// falling back to a guaranteed-readable pair when the brand colour is too
	// light to scan.
	code, err := BuildQRCode(url, QRCodeOptions{
		Size:  360,
		Dark:  sf.Theme.Primary,
		Light: "#ffffff",
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not generate the QR code")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"url":           url,
		"qr":            code,
		"download_name": QRDownloadName(sf.Slug),
		"host":          sf.Slug,
		"published":     sf.IsPublished,
	})
}

// StoreLink returns just the public URL. The shop shell uses it for its
// "View storefront" link.
func (a *AdminHandler) StoreLink(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	url := a.base(sf.Slug)
	response.JSON(w, http.StatusOK, map[string]any{
		"name":          sf.Name,
		"slug":          sf.Slug,
		"public_url":    url,
		"public_host":   url,
		"public_path":   "",
		"is_published":  sf.IsPublished,
		"ordering_open": sf.OrderingAllowed(),
	})
}
