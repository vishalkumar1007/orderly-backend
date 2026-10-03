// Package publicurl builds browser-facing origins for tenants and the platform
// console. These are never API hosts ({slug}.api.{base} / api.{base}) — only
// pages users open in a browser.
package publicurl

import (
	"net/url"
	"strings"
)

// IsDev reports whether APP_ENV should advertise http + Vite-style ports.
func IsDev(appEnv string) bool {
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "", "development", "dev":
		return true
	default:
		return false
	}
}

func schemeAndPort(appEnv, frontendPort string) (scheme, port string) {
	if IsDev(appEnv) {
		port = strings.TrimSpace(frontendPort)
		if port == "80" {
			port = ""
		}
		return "http", port
	}
	return "https", ""
}

func joinOrigin(scheme, host, port string) string {
	if host == "" {
		return ""
	}
	if port != "" && port != "80" && port != "443" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}

// TenantHost is the browser host for a tenant (no scheme), e.g.
// "vm-food.orderly.qd.je" or "vm-food.localhost:5173".
func TenantHost(slug, baseDomain, appEnv, frontendPort string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if slug == "" || baseDomain == "" {
		return ""
	}
	_, port := schemeAndPort(appEnv, frontendPort)
	host := slug + "." + baseDomain
	if port != "" {
		return host + ":" + port
	}
	return host
}

// TenantFrontendURL is the absolute browser origin for a tenant storefront /
// shop console, e.g. "https://vm-food.orderly.qd.je".
func TenantFrontendURL(slug, baseDomain, appEnv, frontendPort string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if slug == "" || baseDomain == "" {
		return ""
	}
	scheme, port := schemeAndPort(appEnv, frontendPort)
	return joinOrigin(scheme, slug+"."+baseDomain, port)
}

// AdminConsoleHost is the host label for the platform console (no scheme).
// Local apex: "localhost:5173". Otherwise: "admin.{baseDomain}".
func AdminConsoleHost(baseDomain, appEnv, frontendPort string) string {
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if baseDomain == "" {
		baseDomain = "localhost"
	}
	_, port := schemeAndPort(appEnv, frontendPort)
	host := "admin." + baseDomain
	if IsDev(appEnv) && (baseDomain == "localhost" || baseDomain == "127.0.0.1") {
		host = "localhost"
	}
	if port != "" {
		return host + ":" + port
	}
	return host
}

// AdminConsoleURL is the absolute origin for the platform superadmin console.
// Local apex (matching existing /superadmin routes on localhost): http://localhost:5173.
// Production: https://admin.{BASE_DOMAIN} when base is not localhost; otherwise https://{BASE_DOMAIN}.
func AdminConsoleURL(baseDomain, appEnv, frontendPort string) string {
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if baseDomain == "" {
		baseDomain = "localhost"
	}
	scheme, port := schemeAndPort(appEnv, frontendPort)
	var host string
	if IsDev(appEnv) && (baseDomain == "localhost" || baseDomain == "127.0.0.1") {
		// Superadmin runs on the Vite apex locally (not admin.localhost).
		host = "localhost"
	} else if !IsDev(appEnv) && (baseDomain == "localhost" || baseDomain == "127.0.0.1") {
		host = baseDomain
	} else {
		// Deployed console is commonly reached via apex; invite emails and
		// settings historically used the apex for /superadmin paths. Prefer
		// apex so links match https://orderly.qd.je/superadmin/...
		host = baseDomain
	}
	return joinOrigin(scheme, host, port)
}

// HostOf strips scheme from an absolute URL for legacy public_host fields.
func HostOf(absolute string) string {
	absolute = strings.TrimSpace(absolute)
	if absolute == "" {
		return ""
	}
	u, err := url.Parse(absolute)
	if err != nil || u.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(absolute, "https://"), "http://")
	}
	return u.Host
}
