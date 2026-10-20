// Package media serves uploaded images through the API's own address.
//
// Storage links are the wrong thing to put in a page: a private bucket only
// hands out links that expire in minutes, and a bucket reached over plain
// http is blocked by browsers on an https page (mixed content), which is why
// images opened fine in their own tab but not in the app. A media URL is
// stable, always on the API's scheme, and works with a private bucket.
package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/internal/configsvc"
)

// PlatformScope names objects in the platform's own storage.
const PlatformScope = "p"

// URL is the stable address of an uploaded object: scope is PlatformScope or
// the owning tenant's id, key is the key it was uploaded under.
func URL(origin, scope, key string) string {
	return strings.TrimRight(origin, "/") + "/api/v1/media/" + scope + "/" + strings.TrimLeft(key, "/")
}

// Only the folders uploads write to are served, never an arbitrary key.
var (
	tenantKey   = regexp.MustCompile(`^(menu-items/[0-9a-f-]{36}|email-assets)/[0-9a-f-]{36}\.(png|jpe?g|webp|gif)$`)
	platformKey = regexp.MustCompile(`^(platform/branding|email-assets)/[0-9a-f-]{36}\.(png|jpe?g|webp|gif|svg)$`)
)

// Allowed reports whether key may be served for scope. A tenant's menu images
// must live under its own id, so one tenant cannot read another's folder.
func Allowed(scope, key string) bool {
	if scope == PlatformScope {
		return platformKey.MatchString(key)
	}
	if _, err := uuid.Parse(scope); err != nil || !tenantKey.MatchString(key) {
		return false
	}
	return !strings.HasPrefix(key, "menu-items/") || strings.HasPrefix(key, "menu-items/"+scope+"/")
}

// Handler streams media from storage.
type Handler struct {
	storage *configsvc.Storage
}

func NewHandler(storage *configsvc.Storage) *Handler { return &Handler{storage: storage} }

// Serve handles GET /media/{scope}/*. Public by necessity (an <img> and a
// mail client fetch anonymously) and narrow by design (see Allowed).
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) {
	scope, key := chi.URLParam(r, "scope"), chi.URLParam(r, "*")
	if h.storage == nil || !Allowed(scope, key) {
		http.NotFound(w, r)
		return
	}
	var tenant *uuid.UUID
	if scope != PlatformScope {
		id := uuid.MustParse(scope)
		tenant = &id
	}
	obj, err := h.storage.Get(r.Context(), tenant, key)
	if err != nil {
		if errors.Is(err, configsvc.ErrObjectNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "media unavailable", http.StatusBadGateway)
		}
		return
	}
	defer obj.Body.Close()

	ctype := obj.ContentType
	if !strings.HasPrefix(ctype, "image/") {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if obj.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	// Keys are unique per upload, so an image never changes under its URL.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// An uploaded SVG opened directly must not run script on the API origin.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	_, _ = io.Copy(w, obj.Body)
}

// storedKey finds the media key inside a storage link (a presigned or public
// bucket URL), so links saved before media URLs existed can be rewritten.
var storedKey = regexp.MustCompile(`/((?:menu-items/([0-9a-f-]{36})|platform/branding|email-assets)/[0-9a-f-]{36}\.(?:png|jpe?g|webp|gif|svg))(?:\?|$)`)

// Rewrite turns a storage link into its media URL. ok is false for anything
// that is not a storage link this package can serve (already a media URL, an
// outside image, empty).
func Rewrite(origin, link string) (string, bool) {
	if link == "" || strings.Contains(link, "/api/v1/media/") {
		return "", false
	}
	m := storedKey.FindStringSubmatch(link)
	if m == nil {
		return "", false
	}
	scope := PlatformScope
	if m[2] != "" {
		scope = m[2]
	}
	if !Allowed(scope, m[1]) {
		return "", false
	}
	return URL(origin, scope, m[1]), true
}

// columns that hold uploaded image links.
var columns = []struct{ table, column, id string }{
	{"products", "image_url", "id"},
	{"categories", "image_url", "id"},
	{"tenants", "logo_url", "id"},
	{"tenants", "favicon_url", "id"},
	{"tenant_storefront_settings", "logo_url", "tenant_id"},
	{"tenant_storefront_settings", "favicon_url", "tenant_id"},
	{"tenant_storefront_settings", "hero_image_url", "tenant_id"},
	{"email_branding", "logo_url", "id"},
}

// Repair rewrites saved storage links to media URLs, once per link: a
// rewritten link no longer matches. Safe to run on every start.
func Repair(ctx context.Context, pool *pgxpool.Pool, origin string) (int, error) {
	fixed := 0
	if pool == nil {
		return 0, nil
	}
	for _, c := range columns {
		rows, err := pool.Query(ctx, "SELECT "+c.id+"::text, "+c.column+" FROM "+c.table+
			" WHERE "+c.column+" <> '' AND "+c.column+" NOT LIKE '%/api/v1/media/%'")
		if err != nil {
			return fixed, err
		}
		type change struct{ id, url string }
		var todo []change
		for rows.Next() {
			var id, link string
			if err := rows.Scan(&id, &link); err == nil {
				if next, ok := Rewrite(origin, link); ok {
					todo = append(todo, change{id, next})
				}
			}
		}
		rows.Close()
		for _, ch := range todo {
			if _, err := pool.Exec(ctx, "UPDATE "+c.table+" SET "+c.column+" = $1 WHERE "+c.id+"::text = $2", ch.url, ch.id); err != nil {
				return fixed, err
			}
			fixed++
		}
	}
	return fixed, nil
}
