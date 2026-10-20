package media

import "testing"

const (
	tenantA = "ee221e44-1920-4b33-9d64-8139d47f0fed"
	tenantB = "11111111-2222-3333-4444-555555555555"
	file    = "61e1101d-8e4a-4a80-a4d6-1bca81a2b3c4"
)

func TestAllowedKeepsTenantsApart(t *testing.T) {
	cases := []struct {
		scope, key string
		want       bool
	}{
		{tenantA, "menu-items/" + tenantA + "/" + file + ".jpg", true},
		{tenantB, "menu-items/" + tenantA + "/" + file + ".jpg", false}, // another tenant's folder
		{tenantA, "email-assets/" + file + ".png", true},
		{PlatformScope, "platform/branding/" + file + ".svg", true},
		{PlatformScope, "menu-items/" + tenantA + "/" + file + ".jpg", false},
		{tenantA, "menu-items/" + tenantA + "/../secret.txt", false},
		{tenantA, "backups/db.sql", false},
		{"not-a-uuid", "email-assets/" + file + ".png", false},
	}
	for _, c := range cases {
		if got := Allowed(c.scope, c.key); got != c.want {
			t.Errorf("Allowed(%q, %q) = %v, want %v", c.scope, c.key, got, c.want)
		}
	}
}

func TestRewriteTurnsStorageLinksIntoMediaURLs(t *testing.T) {
	origin := "https://api.orderly.qd.je"
	presigned := "http://137.23.38.217:9000/orderly-uploads/menu-items/" + tenantA + "/" + file +
		".jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=abc"
	got, ok := Rewrite(origin, presigned)
	want := origin + "/api/v1/media/" + tenantA + "/menu-items/" + tenantA + "/" + file + ".jpg"
	if !ok || got != want {
		t.Fatalf("Rewrite(presigned) = %q, %v; want %q", got, ok, want)
	}
	got, ok = Rewrite(origin, "http://minio:9000/b/platform/branding/"+file+".png")
	if !ok || got != origin+"/api/v1/media/p/platform/branding/"+file+".png" {
		t.Fatalf("platform logo: %q, %v", got, ok)
	}
	for _, keep := range []string{"", "https://cdn.example.com/photo.jpg", want} {
		if _, ok := Rewrite(origin, keep); ok {
			t.Errorf("Rewrite(%q) must leave it alone", keep)
		}
	}
}
