package menu

import (
	"fmt"
	"testing"
	"time"
)

// days builds n consecutive daily points starting at start, with 1 order and
// ₹10.10 each so totals expose float drift.
func days(start string, n int) []DayPoint {
	t, _ := time.Parse("2006-01-02", start)
	out := make([]DayPoint, n)
	for i := range out {
		out[i] = DayPoint{Day: t.AddDate(0, 0, i).Format("2006-01-02"), OrderCount: 1, Revenue: 10.10}
	}
	return out
}

func TestBucketPointsDayIsUnchanged(t *testing.T) {
	in := days("2026-10-01", 10)
	out := bucketPoints(in, "day")
	if len(out) != 10 || out[0] != in[0] {
		t.Fatalf("day granularity must return the series untouched, got %d points", len(out))
	}
	// An unknown value behaves like day rather than failing.
	if len(bucketPoints(in, "fortnight")) != 10 {
		t.Error("an unknown granularity should fall back to the daily series")
	}
}

func TestBucketPointsWeeksStartOnMonday(t *testing.T) {
	// 2026-10-01 is a Thursday. Weeks: Sep 28, Oct 5, Oct 12, Oct 19.
	out := bucketPoints(days("2026-10-01", 21), "week") // Oct 1 .. Oct 21
	var starts []string
	for _, p := range out {
		starts = append(starts, p.Day)
		if wd, _ := time.Parse("2006-01-02", p.Day); wd.Weekday() != time.Monday {
			t.Errorf("week bucket %s starts on %s, want Monday", p.Day, wd.Weekday())
		}
	}
	if fmt.Sprint(starts) != "[2026-09-28 2026-10-05 2026-10-12 2026-10-19]" {
		t.Errorf("week starts = %v", starts)
	}
	// The first and last weeks are partial: Thu-Sun = 4 days, Mon-Wed = 3 days.
	if out[0].OrderCount != 4 || out[3].OrderCount != 3 || out[1].OrderCount != 7 {
		t.Errorf("per-week counts = %d %d %d %d, want 4 7 7 3", out[0].OrderCount, out[1].OrderCount, out[2].OrderCount, out[3].OrderCount)
	}
}

func TestBucketPointsMonthsStartOnTheFirst(t *testing.T) {
	// Spans a month end and a year end: Dec 29 .. Feb 2.
	out := bucketPoints(days("2026-12-29", 36), "month")
	var starts []string
	for _, p := range out {
		starts = append(starts, p.Day)
	}
	if fmt.Sprint(starts) != "[2026-12-01 2027-01-01 2027-02-01]" {
		t.Fatalf("month starts = %v", starts)
	}
	if out[0].OrderCount != 3 || out[1].OrderCount != 31 || out[2].OrderCount != 2 {
		t.Errorf("per-month counts = %d %d %d, want 3 31 2", out[0].OrderCount, out[1].OrderCount, out[2].OrderCount)
	}
}

func TestBucketPointsPreserveTotals(t *testing.T) {
	in := days("2026-02-20", 90) // includes a Feb→Mar boundary
	for _, g := range []string{"week", "month"} {
		var orders int64
		var revenue float64
		for _, p := range bucketPoints(in, g) {
			orders += p.OrderCount
			revenue += p.Revenue
		}
		if orders != 90 {
			t.Errorf("%s: buckets hold %d orders, want 90", g, orders)
		}
		if revenue < 908.99 || revenue > 909.01 {
			t.Errorf("%s: buckets hold ₹%.4f, want ₹909.00", g, revenue)
		}
	}
	for _, p := range bucketPoints(in, "week") {
		if p.Revenue != float64(int64(p.Revenue*100+0.5))/100 {
			t.Errorf("week %s revenue %v is not rounded to paise", p.Day, p.Revenue)
		}
	}
}

func TestBucketPointsEmptyAndBadDates(t *testing.T) {
	if got := bucketPoints(nil, "week"); len(got) != 0 {
		t.Errorf("empty input gave %d buckets", len(got))
	}
	got := bucketPoints([]DayPoint{{Day: "garbage", OrderCount: 5}, {Day: "2026-10-05", OrderCount: 2}}, "week")
	if len(got) != 1 || got[0].OrderCount != 2 {
		t.Errorf("a malformed day must be skipped, not crash or poison a bucket: %+v", got)
	}
}

func TestFulfilmentRate(t *testing.T) {
	if fulfilmentRate(0, 0) != nil {
		t.Error("nothing finished yet must be null, not 0%")
	}
	cases := []struct {
		completed, cancelled int64
		want                 float64
	}{{3, 1, 75}, {1, 0, 100}, {0, 4, 0}, {2, 1, 66.7}}
	for _, c := range cases {
		got := fulfilmentRate(c.completed, c.cancelled)
		if got == nil || *got != c.want {
			t.Errorf("fulfilmentRate(%d, %d) = %v, want %v", c.completed, c.cancelled, got, c.want)
		}
	}
}
