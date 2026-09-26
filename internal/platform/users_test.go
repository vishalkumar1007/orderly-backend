package platform

import "testing"

func TestWouldOrphanTenant(t *testing.T) {
	const (
		admin  = "TENANT_ADMIN"
		staff  = "STAFF"
		active = "ACTIVE"
		off    = "DISABLED"
	)

	cases := []struct {
		name                                             string
		currentRole, currentStatus, nextRole, nextStatus string
		want                                             bool
	}{
		{"disabling the only admin", admin, active, admin, off, true},
		{"demoting the only admin to staff", admin, active, staff, active, true},
		{"leaving the active-admin set at all", admin, active, admin, off, true},
		{"renaming an admin", admin, active, admin, active, false},
		{"re-disabling an already disabled admin", admin, off, admin, off, false},
		{"re-promoting an already active admin", admin, active, admin, active, false},
		{"disabling staff", staff, active, staff, off, false},
		{"promoting staff to admin", staff, active, admin, active, false},
		{"enabling a disabled admin", admin, off, admin, active, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wouldOrphanTenant(tc.currentRole, tc.currentStatus, tc.nextRole, tc.nextStatus)
			if got != tc.want {
				t.Fatalf("wouldOrphanTenant(%q,%q -> %q,%q) = %v, want %v",
					tc.currentRole, tc.currentStatus, tc.nextRole, tc.nextStatus, got, tc.want)
			}
		})
	}
}

func TestOrphanedByRemoval(t *testing.T) {
	cases := []struct {
		activeAdmins int64
		want         bool
	}{
		{0, true},  // target not counted (was not active) and nobody else
		{1, true},  // target is the only admin
		{2, false}, // one other admin survives
		{5, false},
	}

	for _, tc := range cases {
		if got := orphanedByRemoval(tc.activeAdmins); got != tc.want {
			t.Errorf("orphanedByRemoval(%d) = %v, want %v", tc.activeAdmins, got, tc.want)
		}
	}
}
