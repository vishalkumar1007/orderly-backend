package httpserver

import (
	"net/http"
	"testing"

	"github.com/orderly/orderly-backend/internal/config"
)

func TestAllowOrigin_ProductionHTTPS(t *testing.T) {
	s := &Server{cfg: config.Config{
		AppEnv:       "production",
		BaseDomain:   "orderly.qd.je",
		FrontendPort: "5173",
	}}
	req := &http.Request{}

	cases := []struct {
		origin string
		want   bool
	}{
		{"https://orderly.qd.je", true},
		{"https://admin.orderly.qd.je", true},
		{"https://momo-magic.orderly.qd.je", true},
		// Shop frontend Origin when calling the shared https://api.orderly.qd.je
		{"https://vm-food.orderly.qd.je", true},
		{"https://vm-foods.orderly.qd.je", true},
		// API hosts are not browser page Origins.
		{"https://api.orderly.qd.je", false},
		{"https://vm-food.api.orderly.qd.je", false},
		{"https://www.orderly.qd.je", false},
		{"https://evil.example.com", false},
		{"https://not-orderly.qd.je", false},
		{"https://nested.slug.orderly.qd.je", false},
		{"http://localhost:5173", true},
		{"http://admin.orderly.qd.je:5173", true},
		{"http://shop.orderly.qd.je:5173", true},
		// Portful https is not a production browser origin we advertise.
		{"https://orderly.qd.je:443", false},
	}
	for _, tc := range cases {
		if got := s.allowOrigin(req, tc.origin); got != tc.want {
			t.Errorf("allowOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestAllowOrigin_DevPortHop(t *testing.T) {
	s := &Server{cfg: config.Config{
		AppEnv:       "development",
		BaseDomain:   "localhost",
		FrontendPort: "5173",
	}}
	req := &http.Request{}

	if !s.allowOrigin(req, "http://localhost:5174") {
		t.Fatal("expected local Vite alt port to be allowed in development")
	}
	if !s.allowOrigin(req, "http://shop.localhost:5174") {
		t.Fatal("expected tenant Vite alt port to be allowed in development")
	}
}
