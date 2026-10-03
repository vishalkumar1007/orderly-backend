package httpserver

import (
	"strings"
	"testing"

	"github.com/orderly/orderly-backend/internal/config"
)

func TestPublicStoreURL_ProductionUsesTenantFrontendHost(t *testing.T) {
	s := &Server{cfg: config.Config{
		AppEnv:       "production",
		BaseDomain:   "orderly.qd.je",
		FrontendPort: "5173",
	}}
	got := s.publicStoreURL("vm-food")
	want := "https://vm-food.orderly.qd.je"
	if got != want {
		t.Fatalf("publicStoreURL = %q, want %q", got, want)
	}
	if strings.Contains(got, ".api.") {
		t.Fatalf("browser URL must not use API host: %q", got)
	}
}

func TestPublicStoreURL_DevelopmentUsesLocalVitePort(t *testing.T) {
	s := &Server{cfg: config.Config{
		AppEnv:       "development",
		BaseDomain:   "localhost",
		FrontendPort: "5173",
	}}
	got := s.publicStoreURL("vm-food")
	want := "http://vm-food.localhost:5173"
	if got != want {
		t.Fatalf("publicStoreURL = %q, want %q", got, want)
	}
}
