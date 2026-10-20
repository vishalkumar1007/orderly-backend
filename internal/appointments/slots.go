package appointments

import "time"

// span is a half-open time range [start, end).
type span struct {
	start, end time.Time
}

func (s span) overlaps(start, end time.Time) bool {
	return s.start.Before(end) && start.Before(s.end)
}

// window is one staff member's working range on a day, as wall-clock times
// read from staff_availability.
type window struct {
	staffID    string
	start, end time.Duration // offsets from midnight
}

// slot is a bookable start time with the staff member who can take it.
type slot struct {
	Start   time.Time
	StaffID string
}

// computeSlots lists every start time on `day` (midnight in the business's
// timezone) where a service of length dur fits inside a staff member's
// working window without touching their busy time, stepping by step and
// keeping only starts in [earliest, latest]. When several staff can take the
// same start, the first in window order gets it, so each start appears once.
//
// Wall-clock times are built with time.Date in the business's location, so a
// daylight-saving change on that day lands on the right instant.
func computeSlots(day time.Time, windows []window, busy map[string][]span, dur, step time.Duration, earliest, latest time.Time) []slot {
	y, m, d := day.Date()
	loc := day.Location()
	at := func(off time.Duration) time.Time {
		return time.Date(y, m, d, int(off/time.Hour), int(off%time.Hour/time.Minute), 0, 0, loc)
	}
	seen := map[int64]bool{}
	var out []slot
	for _, w := range windows {
		end := at(w.end)
		for t := at(w.start); !t.Add(dur).After(end); t = t.Add(step) {
			if t.Before(earliest) || t.After(latest) || seen[t.Unix()] {
				continue
			}
			free := true
			for _, b := range busy[w.staffID] {
				if b.overlaps(t, t.Add(dur)) {
					free = false
					break
				}
			}
			if free {
				seen[t.Unix()] = true
				out = append(out, slot{Start: t, StaffID: w.staffID})
			}
		}
	}
	// Windows come per staff member, so sort the merged list by time.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
