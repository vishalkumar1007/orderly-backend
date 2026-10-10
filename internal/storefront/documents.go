package storefront

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// parsePayments reads the stored payment document. A document that enables no
// method at all falls back to online payment, so a tenant can never end up in a
// state where no customer can pay.
func parsePayments(raw []byte) PaymentSettings {
	out := PaymentSettings{
		OnlineEnabled:  true,
		CashEnabled:    true,
		AtPickupEnable: true,
		DefaultMethod:  MethodOnline,
	}
	if len(raw) == 0 {
		return out
	}
	var doc struct {
		OnlinePaymentEnabled *bool   `json:"online_payment_enabled"`
		CashEnabled          *bool   `json:"cash_enabled"`
		PayAtPickupEnabled   *bool   `json:"pay_at_pickup_enabled"`
		DefaultPaymentMethod *string `json:"default_payment_method"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	if doc.OnlinePaymentEnabled != nil {
		out.OnlineEnabled = *doc.OnlinePaymentEnabled
	}
	if doc.CashEnabled != nil {
		out.CashEnabled = *doc.CashEnabled
	}
	if doc.PayAtPickupEnabled != nil {
		out.AtPickupEnable = *doc.PayAtPickupEnabled
	}
	if doc.DefaultPaymentMethod != nil {
		out.DefaultMethod = upperOneOf(*doc.DefaultPaymentMethod, MethodOnline, []string{MethodOnline, MethodCash})
	}
	if !out.OnlineEnabled && !out.CashEnabled {
		out.OnlineEnabled = true
	}
	if !out.Allows(out.DefaultMethod) {
		if out.OnlineEnabled {
			out.DefaultMethod = MethodOnline
		} else {
			out.DefaultMethod = MethodCash
		}
	}
	return out
}

// MarshalPayments writes the document back out.
func (p PaymentSettings) Marshal() []byte {
	doc := map[string]any{
		"online_payment_enabled": p.OnlineEnabled,
		"cash_enabled":           p.CashEnabled,
		"pay_at_pickup_enabled":  p.AtPickupEnable,
		"default_payment_method": p.DefaultMethod,
	}
	b, _ := json.Marshal(doc)
	return b
}

// PayOnline reports whether a customer can pay over UPI/card right now.
func (p PaymentSettings) PayOnline() bool { return p.OnlineEnabled }

// PayCash reports whether cash at pickup is available.
func (p PaymentSettings) PayCash() bool { return p.CashEnabled }

// parseWorkflow reads the stored workflow document. The state machine itself is
// fixed in the orders package; this document only tunes behaviour around it.
func parseWorkflow(raw []byte) Workflow {
	out := Workflow{
		AcceptanceMode:     AcceptManual,
		PaymentRequirement: PayBeforePrep,
		ReadyNotification:  true,
		AutoComplete:       false,
		NewOrderSound:      SoundChime,
		OrderReadySound:    SoundChime,
		InAppNewOrder:      true,
		InAppOrderReady:    true,
	}
	if len(raw) == 0 {
		return out
	}
	var doc struct {
		AcceptanceMode     *string `json:"acceptance_mode"`
		PaymentRequirement *string `json:"payment_requirement"`
		ReadyNotification  *bool   `json:"ready_notification"`
		AutoComplete       *bool   `json:"auto_complete"`
		NewOrderSound      *string `json:"new_order_sound"`
		OrderReadySound    *string `json:"order_ready_sound"`
		InAppNewOrder      *bool   `json:"in_app_new_order_enabled"`
		InAppOrderReady    *bool   `json:"in_app_order_ready_enabled"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	if doc.AcceptanceMode != nil {
		out.AcceptanceMode = upperOneOf(*doc.AcceptanceMode, AcceptManual, validAccept)
	}
	if doc.PaymentRequirement != nil {
		out.PaymentRequirement = upperOneOf(*doc.PaymentRequirement, PayBeforePrep, validPayTiming)
	}
	if doc.ReadyNotification != nil {
		out.ReadyNotification = *doc.ReadyNotification
	}
	if doc.AutoComplete != nil {
		out.AutoComplete = *doc.AutoComplete
	}
	if doc.NewOrderSound != nil {
		out.NewOrderSound = upperOneOf(*doc.NewOrderSound, SoundChime, validSounds)
	}
	if doc.OrderReadySound != nil {
		out.OrderReadySound = upperOneOf(*doc.OrderReadySound, SoundChime, validSounds)
	}
	if doc.InAppNewOrder != nil {
		out.InAppNewOrder = *doc.InAppNewOrder
	}
	if doc.InAppOrderReady != nil {
		out.InAppOrderReady = *doc.InAppOrderReady
	}
	return out
}

// Marshal writes the workflow document back out.
func (w Workflow) Marshal() []byte {
	b, _ := json.Marshal(map[string]any{
		"acceptance_mode":            w.AcceptanceMode,
		"payment_requirement":        w.PaymentRequirement,
		"ready_notification":         w.ReadyNotification,
		"auto_complete":              w.AutoComplete,
		"new_order_sound":            w.NewOrderSound,
		"order_ready_sound":          w.OrderReadySound,
		"in_app_new_order_enabled":   w.InAppNewOrder,
		"in_app_order_ready_enabled": w.InAppOrderReady,
	})
	return b
}

// AutoAccept reports whether a new order skips the manual accept step.
func (w Workflow) AutoAccept() bool { return w.AcceptanceMode == AcceptAuto }

// RequiresPaymentUpfront reports whether the shop only starts preparing once
// money has actually been received.
func (w Workflow) RequiresPaymentUpfront() bool {
	return w.PaymentRequirement == PayBeforePrep
}

// ---------------------------------------------------------------------------
// Opening hours
// ---------------------------------------------------------------------------

// dayKeys are the schedule keys. The order matters: it matches the weekday
// cycle starting on Monday.
var dayKeys = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// DayKeys returns the ordered schedule keys, for the admin schedule editor.
func DayKeys() []string { return dayKeys }

// dayLabels are the human names shown next to each schedule key.
var dayLabels = map[string]string{
	"mon": "Monday", "tue": "Tuesday", "wed": "Wednesday",
	"thu": "Thursday", "fri": "Friday", "sat": "Saturday", "sun": "Sunday",
}

// weekdayKey maps time.Weekday (Sunday = 0) onto dayKeys (Monday = 0). Getting
// this wrong shifts every store's opening hours by a day, so it lives in one
// named place with a test.
func weekdayKey(w time.Weekday) string {
	return dayKeys[(int(w)+6)%7]
}

// timeRange is one opening window within a day, as minutes from midnight.
type timeRange struct {
	open  time.Duration
	close time.Duration
}

// Hours is the weekly opening schedule. A day with no ranges is closed.
type Hours struct {
	AlwaysOpen bool                  `json:"always_open"`
	Timezone   string                `json:"timezone"`
	Schedule   map[string][][]string `json:"schedule"`

	// ranges is the indexed form used at request time. It is rebuilt from
	// Schedule whenever the document is parsed, so evaluation never re-parses a
	// clock string.
	ranges   map[string][]timeRange
	location *time.Location
}

// parseHours reads the stored schedule and indexes it for evaluation. Malformed
// times are dropped; a document that yields no usable window is treated as
// always open, which fails safe for a store that has not finished setup.
func parseHours(raw []byte, timezone string) Hours {
	h := Hours{
		AlwaysOpen: true,
		Timezone:   timezone,
		Schedule:   map[string][][]string{},
		ranges:     map[string][]timeRange{},
	}
	if timezone == "" {
		timezone = "UTC"
	}
	h.location, _ = time.LoadLocation(timezone)

	if len(raw) > 0 {
		var doc struct {
			AlwaysOpen *bool          `json:"always_open"`
			Timezone   string         `json:"timezone"`
			Schedule   map[string]any `json:"schedule"`
		}
		if err := json.Unmarshal(raw, &doc); err == nil {
			if doc.AlwaysOpen != nil {
				h.AlwaysOpen = *doc.AlwaysOpen
			}
			if tz := strings.TrimSpace(doc.Timezone); tz != "" {
				h.Timezone = tz
				if loc, err := time.LoadLocation(tz); err == nil {
					h.location = loc
				}
			}
			if doc.Schedule != nil {
				h.Schedule = normaliseSchedule(doc.Schedule)
			}
		}
	}
	if !h.AlwaysOpen && len(h.Schedule) == 0 {
		// A schedule with no usable entries would read as "never open", which
		// silently kills ordering. Treat it as unset instead.
		h.AlwaysOpen = true
	}
	h.index()
	return h
}

// HoursConfigured reports whether an admin has explicitly saved opening hours.
// Virgin default '{}' parses as always-open for ordering but is not "configured"
// for the launch checklist — otherwise every new shop would look finished.
func HoursConfigured(raw []byte) bool {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("{}")) || bytes.Equal(trim, []byte("null")) {
		return false
	}
	var doc struct {
		AlwaysOpen *bool          `json:"always_open"`
		Schedule   map[string]any `json:"schedule"`
	}
	if err := json.Unmarshal(trim, &doc); err != nil {
		return false
	}
	if doc.AlwaysOpen != nil {
		return true
	}
	if doc.Schedule == nil {
		return false
	}
	return len(normaliseSchedule(doc.Schedule)) > 0
}

// HoursConfigured reports whether this storefront has explicitly saved opening hours.
func (s *Storefront) HoursConfigured() bool {
	return HoursConfigured(s.rawOpeningHours)
}

// normaliseSchedule accepts both shapes an admin might hand us:
//   - "mon": ["09:00", "22:00"]                      (one range, flat)
//   - "mon": [["09:00","12:00"], ["17:00","22:00"]]  (split shifts)
//
// Anything that is not a valid, forward-running clock range is dropped here
// rather than stored, so the schedule an admin sees is exactly the schedule the
// storefront enforces.
func normaliseSchedule(in map[string]any) map[string][][]string {
	out := map[string][][]string{}
	for _, day := range dayKeys {
		raw, ok := in[day]
		if !ok {
			continue
		}
		list, ok := raw.([]any)
		if !ok {
			continue
		}
		// Flat form: two clock strings.
		if len(list) == 2 {
			if pair, ok := clockRange(list[0], list[1]); ok {
				out[day] = append(out[day], pair)
				continue
			}
		}
		// Nested form: each entry is a two-clock range.
		for _, entry := range list {
			pair, ok := entry.([]any)
			if !ok {
				continue
			}
			if r, ok := clockRange(pair[0], pair[1]); ok {
				out[day] = append(out[day], r)
			}
		}
	}
	return out
}

func clockRange(a, b any) ([]string, bool) {
	first, ok1 := a.(string)
	second, ok2 := b.(string)
	if !ok1 || !ok2 || !isClock(first) || !isClock(second) {
		return nil, false
	}
	// A window that ends before it starts cannot be evaluated, so it is dropped
	// rather than stored and silently ignored later.
	if second <= first {
		return nil, false
	}
	return []string{first, second}, true
}

// isClock reports whether a value is a 24-hour "HH:MM" time.
func isClock(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	for i, r := range s {
		if i == 2 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return s[0:2] <= "23" && s[3:5] <= "59"
}

// index converts the stored clock strings into minute offsets.
func (h *Hours) index() {
	h.ranges = map[string][]timeRange{}
	for _, day := range dayKeys {
		for _, r := range h.Schedule[day] {
			open, err1 := time.Parse("15:04", r[0])
			clos, err2 := time.Parse("15:04", r[1])
			if err1 != nil || err2 != nil {
				continue
			}
			h.ranges[day] = append(h.ranges[day], timeRange{
				open:  time.Duration(open.Hour()*60+open.Minute()) * time.Minute,
				close: time.Duration(clos.Hour()*60+clos.Minute()) * time.Minute,
			})
		}
	}
}

// IsOpen reports whether the store is inside an opening window at time t,
// evaluated in the tenant's own timezone.
func (h Hours) IsOpen(t time.Time) bool {
	if h.AlwaysOpen {
		return true
	}
	local := t.In(h.locationOrUTC())
	day := weekdayKey(local.Weekday())
	minute := time.Duration(local.Hour()*60+local.Minute()) * time.Minute
	for _, r := range h.ranges[day] {
		if minute >= r.open && minute < r.close {
			return true
		}
	}
	return false
}

// Status is the customer-facing open/closed state.
type Status struct {
	Open        bool   `json:"is_open"`
	Label       string `json:"label"`
	Detail      string `json:"detail"`
	NextChange  string `json:"next_change,omitempty"`
	AlwaysOpen  bool   `json:"always_open"`
	Timezone    string `json:"timezone"`
	TodayCloses string `json:"today_closes,omitempty"`
}

// Status computes the current state plus a human label the header badge and the
// closed banner can both use.
func (h Hours) Status(now time.Time) Status {
	local := now.In(h.locationOrUTC())
	today := weekdayKey(local.Weekday())
	minute := time.Duration(local.Hour()*60+local.Minute()) * time.Minute
	st := Status{AlwaysOpen: h.AlwaysOpen, Timezone: h.Timezone}

	if h.AlwaysOpen {
		st.Open = true
		st.Label = "Open"
		st.Detail = "Open 24 hours"
		st.TodayCloses = "24 hours"
		return st
	}

	todayRanges := h.sorted(today)
	for _, r := range todayRanges {
		if minute >= r.open && minute < r.close {
			st.Open = true
			st.Label = "Open"
			// A split shift should say when the current window ends, not when
			// the whole day does.
			st.TodayCloses = clock(r.close)
			st.Detail = "Open until " + clock(r.close)
			if len(todayRanges) > 1 {
				st.Detail += " · closed for a break, back at " + clock(r.close)
			}
			return st
		}
	}
	// Closed: point at the next opening today, and say which day it is on so
	// "opens 09:00" at 20:00 is not mistaken for this morning.
	st.Open = false
	st.Label = "Closed"
	if next, day, ok := h.nextWindow(today, minute); ok {
		st.Detail = "Closed · opens " + clock(next.open)
		if day != today {
			st.Detail += " " + dayLabels[day]
		}
		st.NextChange = clock(next.open)
		return st
	}
	st.Detail = "Closed today"
	return st
}

// sorted returns a day's windows ordered by opening time. The sort is an
// insertion sort because a day realistically has one or two windows, and this
// runs on the request path.
func (h Hours) sorted(day string) []timeRange {
	ranges := append([]timeRange(nil), h.ranges[day]...)
	for i := 1; i < len(ranges); i++ {
		for j := i; j > 0 && ranges[j].open < ranges[j-1].open; j-- {
			ranges[j], ranges[j-1] = ranges[j-1], ranges[j]
		}
	}
	return ranges
}

// nextWindow finds the next window to open and the day it falls on, looking at
// the rest of today first and then the days that follow.
func (h Hours) nextWindow(today string, now time.Duration) (timeRange, string, bool) {
	for _, r := range h.sorted(today) {
		if r.open > now {
			return r, today, true
		}
	}
	for offset := 1; offset <= len(dayKeys); offset++ {
		key := dayKeys[(indexOf(today)+offset)%len(dayKeys)]
		if ranges := h.sorted(key); len(ranges) > 0 {
			return ranges[0], key, true
		}
	}
	return timeRange{}, "", false
}

func (h Hours) locationOrUTC() *time.Location {
	if h.location == nil {
		return time.UTC
	}
	return h.location
}

func indexOf(day string) int {
	for i, d := range dayKeys {
		if d == day {
			return i
		}
	}
	return 0
}

// Marshal writes the schedule document back out.
func (h Hours) Marshal() []byte {
	schedule := h.Schedule
	if schedule == nil {
		schedule = map[string][][]string{}
	}
	b, _ := json.Marshal(map[string]any{
		"always_open": h.AlwaysOpen,
		"timezone":    h.Timezone,
		"schedule":    schedule,
	})
	return b
}

// clock renders minutes-from-midnight as "HH:MM".
func clock(d time.Duration) string {
	total := int(d / time.Minute)
	return pad2(total/60) + ":" + pad2(total%60)
}

func pad2(n int) string {
	if n < 0 {
		n = 0
	}
	if n > 99 {
		n = 99
	}
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
