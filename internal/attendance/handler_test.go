package attendance

import (
	"math"
	"testing"
)

func TestDistance(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lng1, lat2, lng2 float64
		want, tol              float64
	}{
		{"same point", 12.9716, 77.5946, 12.9716, 77.5946, 0, 0.01},
		// One degree of latitude is ~111.2 km everywhere.
		{"one degree north", 0, 0, 1, 0, 111195, 50},
		// Bengaluru to Chennai is ~290 km.
		{"bengaluru to chennai", 12.9716, 77.5946, 13.0827, 80.2707, 290000, 5000},
		// ~100 m east at the equator.
		{"hundred metres", 0, 0, 0, 0.0008993, 100, 1},
	}
	for _, c := range cases {
		got := Distance(c.lat1, c.lng1, c.lat2, c.lng2)
		if math.Abs(got-c.want) > c.tol {
			t.Errorf("%s: got %.1f m, want %.1f m", c.name, got, c.want)
		}
	}
}
