package tenantctx

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseHost_SlugAPISubdomain(t *testing.T) {
	cases := []struct {
		host string
		kind HostKind
		slug string
	}{
		{"api.orderly.qd.je", HostAPI, ""},
		{"vm-food.api.orderly.qd.je", HostTenant, "vm-food"},
		{"momo-magic.api.orderly.qd.je", HostTenant, "momo-magic"},
		{"vm-food.orderly.qd.je", HostTenant, "vm-food"},
		{"a.b.api.orderly.qd.je", HostUnknown, ""},
		{"api.localhost", HostAPI, ""},
		{"vm-food.api.localhost", HostTenant, "vm-food"},
	}
	for _, tc := range cases {
		base := "orderly.qd.je"
		if strings.HasSuffix(tc.host, ".localhost") || tc.host == "api.localhost" {
			base = "localhost"
		}
		info := ParseHost(tc.host, base)
		if info.Kind != tc.kind || info.Slug != tc.slug {
			t.Fatalf("ParseHost(%q) = kind=%s slug=%q, want kind=%s slug=%q",
				tc.host, info.Kind, info.Slug, tc.kind, tc.slug)
		}
	}
}

func TestEffectiveHost_ForwardedForAPIHost(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://api.orderly.qd.je/api/v1/public/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.orderly.qd.je"
	req.Header.Set("X-Forwarded-Host", "momo-magic.orderly.qd.je")

	got := EffectiveHost(req, "orderly.qd.je")
	if got != "momo-magic.orderly.qd.je" {
		t.Fatalf("EffectiveHost = %q, want tenant host", got)
	}
	info := ParseHost(got, "orderly.qd.je")
	if info.Kind != HostTenant || info.Slug != "momo-magic" {
		t.Fatalf("ParseHost(%q) = %+v", got, info)
	}
}

func TestEffectiveHost_IgnoresForwardedOnTenantHost(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://shop.orderly.qd.je/api/v1/public/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "shop.orderly.qd.je"
	req.Header.Set("X-Forwarded-Host", "other.orderly.qd.je")

	got := EffectiveHost(req, "orderly.qd.je")
	if got != "shop.orderly.qd.je" {
		t.Fatalf("EffectiveHost = %q, want original tenant host", got)
	}
}

func TestEffectiveHost_InternalDockerUpstream(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://fs-A1-d3e4-k9:8080/api/v1/public/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "fs-A1-d3e4-k9:8080"
	req.Header.Set("X-Forwarded-Host", "shop.orderly.qd.je")

	got := EffectiveHost(req, "orderly.qd.je")
	if got != "shop.orderly.qd.je" {
		t.Fatalf("EffectiveHost = %q, want forwarded shop host", got)
	}
}

func TestResolveTenantBinding_SlugHeaderOnAPIHost(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://api.orderly.qd.je/api/v1/public/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.orderly.qd.je"
	req.Header.Set(TenantSlugHeader, "vm-food")

	info := ResolveTenantBinding(req, "orderly.qd.je")
	if info.Kind != HostTenant || info.Slug != "vm-food" {
		t.Fatalf("ResolveTenantBinding = %+v, want HostTenant vm-food", info)
	}
}

func TestResolveTenantBinding_OriginFallback(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://api.orderly.qd.je/api/v1/public/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.orderly.qd.je"
	req.Header.Set("Origin", "https://vm-foods.orderly.qd.je")

	info := ResolveTenantBinding(req, "orderly.qd.je")
	if info.Kind != HostTenant || info.Slug != "vm-foods" {
		t.Fatalf("ResolveTenantBinding = %+v, want HostTenant vm-foods", info)
	}
}

func TestResolveTenantBinding_HostWinsOverForgedSlug(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://shop.orderly.qd.je/api/v1/public/theme", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "shop.orderly.qd.je"
	req.Header.Set(TenantSlugHeader, "other")

	info := ResolveTenantBinding(req, "orderly.qd.je")
	if info.Kind != HostTenant || info.Slug != "shop" {
		t.Fatalf("ResolveTenantBinding = %+v, want HostTenant shop", info)
	}
}

func TestResolveTenantBinding_PlatformAPIWithoutSlug(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://api.orderly.qd.je/api/v1/admin/tenants", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.orderly.qd.je"

	info := ResolveTenantBinding(req, "orderly.qd.je")
	if info.Kind != HostAPI || info.Slug != "" {
		t.Fatalf("ResolveTenantBinding = %+v, want HostAPI", info)
	}
}

func TestNormalizeTenantSlug(t *testing.T) {
	if got := NormalizeTenantSlug("VM-Food"); got != "vm-food" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeTenantSlug("a.b"); got != "" {
		t.Fatalf("nested slug should be rejected, got %q", got)
	}
	if got := NormalizeTenantSlug("Bad_Slug"); got != "" {
		t.Fatalf("invalid slug should be rejected, got %q", got)
	}
}
