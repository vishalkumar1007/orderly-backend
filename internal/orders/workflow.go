package orders

import "fmt"

// Order lifecycle states. The set is closed and the legal moves are fixed in
// allowedTransitions below. Tenant configuration can only tune behaviour around
// this machine — it can never introduce a state or an unsafe move.
const (
	StatusPending   = "PENDING"
	StatusAccepted  = "ACCEPTED"
	StatusPreparing = "PREPARING"
	StatusReady     = "READY"
	StatusCompleted = "COMPLETED"
	StatusCancelled = "CANCELLED"
)

// Payment states. Only the backend moves these.
const (
	PaymentPending  = "PENDING"
	PaymentPaid     = "PAID"
	PaymentFailed   = "FAILED"
	PaymentRefunded = "REFUNDED"
)

// Order type. Delivery is deliberately not part of this build.
const OrderTypePickup = "PICKUP"

// allowedTransitions is the single source of truth for order movement.
//
//	PENDING   → ACCEPTED, CANCELLED
//	ACCEPTED  → PREPARING, CANCELLED
//	PREPARING → READY
//	READY     → COMPLETED
//	COMPLETED → (terminal)
//	CANCELLED → (terminal)
var allowedTransitions = map[string][]string{
	StatusPending:   {StatusAccepted, StatusCancelled},
	StatusAccepted:  {StatusPreparing, StatusCancelled},
	StatusPreparing: {StatusReady},
	StatusReady:     {StatusCompleted},
	StatusCompleted: {},
	StatusCancelled: {},
}

// CanTransition reports whether a move is structurally legal.
func CanTransition(from, to string) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// NextStatuses lists the moves available from a state, for the staff UI.
func NextStatuses(from string) []string {
	out := make([]string, 0, 2)
	out = append(out, allowedTransitions[from]...)
	return out
}

// IsCancellable reports whether a customer or staff member may cancel here.
func IsCancellable(status string) bool {
	return CanTransition(status, StatusCancelled)
}

// IsActive reports whether an order is still in the kitchen pipeline.
func IsActive(status string) bool {
	switch status {
	case StatusPending, StatusAccepted, StatusPreparing, StatusReady:
		return true
	}
	return false
}

// StatusLabel is the customer-facing wording for a state.
func StatusLabel(status string) string {
	switch status {
	case StatusPending:
		return "Order placed"
	case StatusAccepted:
		return "Accepted by the kitchen"
	case StatusPreparing:
		return "Preparing your order"
	case StatusReady:
		return "Ready for pickup"
	case StatusCompleted:
		return "Collected — thank you"
	case StatusCancelled:
		return "Cancelled"
	}
	return status
}

// TimelineStep is one entry in the customer-facing progress timeline.
type TimelineStep struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// State is DONE, CURRENT or UPCOMING.
	State string `json:"state"`
	At    string `json:"at,omitempty"`
}

var timelineOrder = []string{
	StatusPending, StatusAccepted, StatusPreparing, StatusReady, StatusCompleted,
}

var timelineLabels = map[string]string{
	StatusPending:   "Order placed",
	StatusAccepted:  "Accepted",
	StatusPreparing: "Preparing",
	StatusReady:     "Ready",
	StatusCompleted: "Completed",
}

// Timeline renders the fixed five-step progress list. `times` maps a state to
// the moment the order reached it, which is what lets a cancelled order still
// show how far it got before it stopped.
func Timeline(status string, times map[string]string) []TimelineStep {
	steps := make([]TimelineStep, 0, len(timelineOrder))

	current := -1
	if status != StatusCancelled {
		for i, s := range timelineOrder {
			if s == status {
				current = i
				break
			}
		}
	} else {
		// Cancelled: the furthest stage with a recorded timestamp is where it
		// stopped. Fall back to "placed" so the first tick is always shown.
		current = 0
		for i, s := range timelineOrder {
			if times[s] != "" {
				current = i
			}
		}
	}

	for i, s := range timelineOrder {
		state := "UPCOMING"
		switch {
		case i < current:
			state = "DONE"
		case i == current:
			// A cancelled order has no live step, so the last stage it reached
			// is shown as done rather than in progress.
			if status == StatusCancelled {
				state = "DONE"
			} else {
				state = "CURRENT"
			}
		}
		steps = append(steps, TimelineStep{
			Key:   s,
			Label: timelineLabels[s],
			State: state,
			At:    times[s],
		})
	}
	return steps
}

// ErrInvalidTransition is returned when a move is not legal from the current
// state, or when a workflow gate blocks it.
type ErrInvalidTransition struct {
	From    string
	To      string
	Reason  string
	Allowed []string
}

func (e *ErrInvalidTransition) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return fmt.Sprintf("cannot move an order from %s to %s", e.From, e.To)
}

// Message is the customer-safe explanation.
func (e *ErrInvalidTransition) Message() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "This order cannot be updated right now."
}
