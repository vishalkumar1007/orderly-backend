package platform

import (
	"encoding/json"
	"testing"

	"github.com/orderly/orderly-backend/db/sqlc"
)

func TestPlanOffersBusinessType(t *testing.T) {
	planWith := func(businessTypes []string) sqlc.Plan {
		features, _ := json.Marshal(map[string]any{"business_types": businessTypes})
		return sqlc.Plan{Name: "PRO", Features: features}
	}

	cases := []struct {
		name          string
		businessTypes []string
		requested     string
		want          bool
	}{
		{"unrestricted plan offers every type", nil, "GROCERY", true},
		{"restricted plan offers a listed type", []string{"CAFE", "RESTAURANT"}, "cafe", true},
		{"restricted plan rejects an unlisted type", []string{"CAFE", "RESTAURANT"}, "HOTEL", false},
		{"empty business type always passes", []string{"CAFE"}, "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planOffersBusinessType(planWith(c.businessTypes), c.requested)
			if got != c.want {
				t.Errorf("planOffersBusinessType(%v, %q) = %v, want %v", c.businessTypes, c.requested, got, c.want)
			}
		})
	}
}
