package publicurl

import (
	"strings"
	"testing"
)

func TestTenantFrontendURL_Production(t *testing.T) {
	got := TenantFrontendURL("vm-food", "orderly.qd.je", "production", "5173")
	want := "https://vm-food.orderly.qd.je"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, ".api.") {
		t.Fatalf("browser URL must not use API host: %q", got)
	}
}

func TestTenantFrontendURL_Development(t *testing.T) {
	got := TenantFrontendURL("vm-food", "localhost", "development", "5173")
	want := "http://vm-food.localhost:5173"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTenantFrontendURL_DevPort80Omitted(t *testing.T) {
	got := TenantFrontendURL("vm-food", "localhost", "development", "80")
	want := "http://vm-food.localhost"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTenantHost(t *testing.T) {
	if got := TenantHost("VM-Food", "orderly.qd.je", "production", "5173"); got != "vm-food.orderly.qd.je" {
		t.Fatalf("prod host = %q", got)
	}
	if got := TenantHost("vm-food", "localhost", "development", "5173"); got != "vm-food.localhost:5173" {
		t.Fatalf("dev host = %q", got)
	}
}

func TestAdminConsoleURL(t *testing.T) {
	if got := AdminConsoleURL("localhost", "development", "5173"); got != "http://localhost:5173" {
		t.Fatalf("dev console = %q", got)
	}
	if got := AdminConsoleURL("orderly.qd.je", "production", "5173"); got != "https://orderly.qd.je" {
		t.Fatalf("prod console = %q", got)
	}
	if strings.Contains(AdminConsoleURL("orderly.qd.je", "production", "5173"), ".api.") {
		t.Fatal("console URL must not use API host")
	}
}

func TestAdminConsoleHost(t *testing.T) {
	if got := AdminConsoleHost("localhost", "development", "5173"); got != "localhost:5173" {
		t.Fatalf("dev admin_host = %q", got)
	}
	if got := AdminConsoleHost("orderly.qd.je", "production", "5173"); got != "admin.orderly.qd.je" {
		t.Fatalf("prod admin_host = %q", got)
	}
}

func TestHostOf(t *testing.T) {
	if got := HostOf("https://vm-food.orderly.qd.je/login"); got != "vm-food.orderly.qd.je" {
		t.Fatalf("HostOf = %q", got)
	}
}
