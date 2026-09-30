// Package hotel is the Hotel capability group: room types, rooms,
// reservations, folios/charges and housekeeping.
//
// Reservation and room status are two separate state machines, deliberately
// not merged (POC: "Reservation and room state machines remain separate") —
// a room can go to CLEANING while its reservation is already CHECKED_OUT, and
// a room can be OCCUPIED by a walk-in stay that never had a reservation at
// all.
package hotel

// Reservation lifecycle states (POC flow: Reservation → Check-in → Stay →
// Check-out).
const (
	ReservationRequested  = "REQUESTED"
	ReservationConfirmed  = "CONFIRMED"
	ReservationCheckedIn  = "CHECKED_IN"
	ReservationCheckedOut = "CHECKED_OUT"
	ReservationCancelled  = "CANCELLED"
	ReservationNoShow     = "NO_SHOW"
)

var reservationTransitions = map[string][]string{
	ReservationRequested:  {ReservationConfirmed, ReservationCancelled},
	ReservationConfirmed:  {ReservationCheckedIn, ReservationCancelled, ReservationNoShow},
	ReservationCheckedIn:  {ReservationCheckedOut},
	ReservationCheckedOut: {},
	ReservationCancelled:  {},
	ReservationNoShow:     {},
}

// CanTransitionReservation reports whether a reservation move is legal.
func CanTransitionReservation(from, to string) bool {
	for _, allowed := range reservationTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Room lifecycle states (POC): AVAILABLE → RESERVED → OCCUPIED →
// CHECKOUT_PENDING → CLEANING → INSPECTION → AVAILABLE.
const (
	RoomAvailable       = "AVAILABLE"
	RoomReserved        = "RESERVED"
	RoomOccupied        = "OCCUPIED"
	RoomCheckoutPending = "CHECKOUT_PENDING"
	RoomCleaning        = "CLEANING"
	RoomInspection      = "INSPECTION"
)

var roomTransitions = map[string][]string{
	RoomAvailable:       {RoomReserved, RoomOccupied}, // OCCUPIED direct: a walk-in stay with no reservation
	RoomReserved:        {RoomOccupied, RoomAvailable},
	RoomOccupied:        {RoomCheckoutPending},
	RoomCheckoutPending: {RoomCleaning},
	RoomCleaning:        {RoomInspection},
	RoomInspection:      {RoomAvailable},
}

// CanTransitionRoom reports whether a room move is legal.
func CanTransitionRoom(from, to string) bool {
	for _, allowed := range roomTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
