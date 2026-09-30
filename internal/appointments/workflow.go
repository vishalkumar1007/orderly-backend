// Package appointments is the Barber Shop capability group: services, staff
// availability, appointments and the shared walk-in/checked-in queue.
package appointments

// Appointment lifecycle states (POC):
//
//	REQUESTED → CONFIRMED → CHECKED_IN → WAITING → IN_SERVICE → COMPLETED
//	CONFIRMED → CANCELLED
//	CONFIRMED → NO_SHOW
//
// The set is closed and the legal moves are fixed below, mirroring
// internal/orders/workflow.go.
const (
	StatusRequested = "REQUESTED"
	StatusConfirmed = "CONFIRMED"
	StatusCheckedIn = "CHECKED_IN"
	StatusWaiting   = "WAITING"
	StatusInService = "IN_SERVICE"
	StatusCompleted = "COMPLETED"
	StatusCancelled = "CANCELLED"
	StatusNoShow    = "NO_SHOW"
)

var allowedTransitions = map[string][]string{
	StatusRequested: {StatusConfirmed, StatusCancelled},
	StatusConfirmed: {StatusCheckedIn, StatusCancelled, StatusNoShow},
	StatusCheckedIn: {StatusWaiting},
	StatusWaiting:   {StatusInService},
	StatusInService: {StatusCompleted},
	StatusCompleted: {},
	StatusCancelled: {},
	StatusNoShow:    {},
}

// CanTransition reports whether an appointment move is legal.
func CanTransition(from, to string) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Queue lifecycle (POC walk-in flow): WAITING → CALLED → IN_SERVICE →
// COMPLETED. A checked-in appointment customer joins the same queue at
// WAITING, so this machine serves both walk-ins and appointments.
const (
	QueueWaiting   = "WAITING"
	QueueCalled    = "CALLED"
	QueueInService = "IN_SERVICE"
	QueueCompleted = "COMPLETED"
	QueueCancelled = "CANCELLED"
)

var queueTransitions = map[string][]string{
	QueueWaiting:   {QueueCalled, QueueCancelled},
	QueueCalled:    {QueueInService, QueueCancelled},
	QueueInService: {QueueCompleted},
	QueueCompleted: {},
	QueueCancelled: {},
}

// CanTransitionQueue reports whether a queue entry move is legal.
func CanTransitionQueue(from, to string) bool {
	for _, allowed := range queueTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
