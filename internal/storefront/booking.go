package storefront

// Booking rules shared by the storefront appointment and stay endpoints and
// sent to the customer app in /public/store. They are platform constants for
// now; a per-business setting can replace them when an owner needs one.
const (
	SlotStepMinutes     = 15
	LeadTimeMinutes     = 60
	HorizonDays         = 30
	CancelWindowMinutes = 120
	MaxNights           = 30
	StayHorizonDays     = 365
)

// BookingSettings is the public shape of the rules above.
func BookingSettings() map[string]any {
	return map[string]any{
		"slot_step_minutes":     SlotStepMinutes,
		"lead_time_minutes":     LeadTimeMinutes,
		"horizon_days":          HorizonDays,
		"cancel_window_minutes": CancelWindowMinutes,
		"max_nights":            MaxNights,
		"requires_sign_in":      true,
		"payment":               "PAY_AT_VENUE",
	}
}
