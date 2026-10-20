package appointments

import (
	"testing"
	"time"
)

func hm(h, m int) time.Duration { return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute }

func starts(s []slot) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Start.Format("15:04") + "/" + x.StaffID
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestComputeSlots(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, ist)
	open := day.Add(-time.Hour)
	far := day.Add(48 * time.Hour)

	t.Run("steps through a window and stops where the service no longer fits", func(t *testing.T) {
		got := computeSlots(day, []window{{"a", hm(9, 0), hm(10, 0)}}, nil, 30*time.Minute, 15*time.Minute, open, far)
		eq(t, starts(got), []string{"09:00/a", "09:15/a", "09:30/a"})
	})

	t.Run("skips anything that touches a booking", func(t *testing.T) {
		busy := map[string][]span{"a": {{day.Add(hm(9, 30)), day.Add(hm(10, 0))}}}
		got := computeSlots(day, []window{{"a", hm(9, 0), hm(11, 0)}}, busy, 30*time.Minute, 30*time.Minute, open, far)
		eq(t, starts(got), []string{"09:00/a", "10:00/a", "10:30/a"})
	})

	t.Run("respects the earliest bookable time", func(t *testing.T) {
		got := computeSlots(day, []window{{"a", hm(9, 0), hm(11, 0)}}, nil, time.Hour, 30*time.Minute, day.Add(hm(9, 40)), far)
		eq(t, starts(got), []string{"10:00/a"})
	})

	t.Run("two windows and two staff give each start once, in time order", func(t *testing.T) {
		windows := []window{{"b", hm(13, 0), hm(14, 0)}, {"a", hm(9, 0), hm(10, 0)}, {"a", hm(13, 0), hm(14, 0)}}
		got := computeSlots(day, windows, nil, time.Hour, time.Hour, open, far)
		eq(t, starts(got), []string{"09:00/a", "13:00/b"})
	})

	t.Run("a busy staff member hands the start to a free one", func(t *testing.T) {
		busy := map[string][]span{"a": {{day.Add(hm(9, 0)), day.Add(hm(10, 0))}}}
		windows := []window{{"a", hm(9, 0), hm(10, 0)}, {"b", hm(9, 0), hm(10, 0)}}
		got := computeSlots(day, windows, busy, time.Hour, time.Hour, open, far)
		eq(t, starts(got), []string{"09:00/b"})
	})

	t.Run("wall-clock times survive a daylight-saving change", func(t *testing.T) {
		ny, _ := time.LoadLocation("America/New_York")
		// 2026-03-08: clocks jump from 02:00 to 03:00.
		dst := time.Date(2026, 3, 8, 0, 0, 0, 0, ny)
		got := computeSlots(dst, []window{{"a", hm(9, 0), hm(10, 0)}}, nil, time.Hour, time.Hour, dst.Add(-time.Hour), dst.Add(48*time.Hour))
		if len(got) != 1 || got[0].Start.Hour() != 9 {
			t.Fatalf("want one 09:00 local start, got %v", got)
		}
	})
}
