package storefront

import (
	"net/http"
	neturl "net/url"
	"strings"

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

// storeLinkHostAndPath splits an absolute public URL into host (with port) and
// path so legacy clients that assemble `http://${host}${path}` stay correct.
// public_url remains the canonical absolute address.
func storeLinkHostAndPath(absolute string) (host, path string) {
	path = "/"
	u, err := neturl.Parse(strings.TrimSpace(absolute))
	if err != nil || u.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(absolute, "https://"), "http://"), path
	}
	host = u.Host
	if u.Path != "" && u.Path != "/" {
		path = u.Path
	}
	return host, path
}

// StoreLink returns the public URL plus live ops fields the shop dashboard
// needs for its hero (publish badge, status toggles, identity).
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
	host, path := storeLinkHostAndPath(url)
	response.JSON(w, http.StatusOK, map[string]any{
		"name":               sf.Name,
		"slug":               sf.Slug,
		"public_url":         url,
		"public_host":        host,
		"public_path":        path,
		"is_published":       sf.IsPublished,
		"ordering_open":      sf.OrderingAllowed(),
		"store_status":       sf.StoreStatus,
		"status_message":     sf.StatusMessage,
		"store_status_label": sf.StoreStatusLabel(),
	})
}
