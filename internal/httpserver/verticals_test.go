package httpserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Hotel and appointments were the thinnest-tested parts of the platform. These
// walk their real flows end to end through the API: including the refusals and
// the isolation between businesses: so a change to either is caught.

type biz struct {
	env   *testEnv
	id    uuid.UUID
	admin string
}

func (b biz) do(method, path string, body any) (int, map[string]any) {
	return b.env.doAs(method, "/api/v1/tenant"+path, b.admin, body, b.id)
}

func (b biz) must(want int, label, method, path string, body any) map[string]any {
	b.env.t.Helper()
	status, resp := b.do(method, path, body)
	b.env.mustStatus(want, status, label, resp)
	return resp
}

func newBiz(env *testEnv, kind string) biz {
	id := env.createTenantOfType("Vertical "+kind, "v-"+strings.ToLower(kind)+"-"+randSuffix(), kind)
	return biz{env: env, id: id, admin: env.tenantAdmin(id)}
}

func idOf(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, _ := m[key].(string)
	if s == "" {
		t.Fatalf("response has no %q: %v", key, m["__raw"])
	}
	return s
}

func listOf(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s is %T, want an array: %v", key, m[key], m["__raw"])
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

func TestHotelStayFromBookingToCheckOut(t *testing.T) {
	env := newTestEnv(t)
	h := newBiz(env, "HOTEL")
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }

	// Setup: room type and room, with validation.
	h.must(http.StatusBadRequest, "room type needs a name", http.MethodPost, "/room-types", map[string]any{"base_price": 100})
	rt := h.must(http.StatusCreated, "create room type", http.MethodPost, "/room-types", map[string]any{"name": "Deluxe", "base_price": 3000, "max_guests": 2})
	rtID := idOf(t, rt, "id")
	h.must(http.StatusBadRequest, "room with no number", http.MethodPost, "/rooms", map[string]any{"room_type_id": rtID})
	h.must(http.StatusBadRequest, "room of an unknown type", http.MethodPost, "/rooms", map[string]any{"room_type_id": uuid.NewString(), "number": "9"})
	room := h.must(http.StatusCreated, "create room", http.MethodPost, "/rooms", map[string]any{"room_type_id": rtID, "number": "101"})
	roomID := idOf(t, room, "id")
	if room["status"] != "AVAILABLE" {
		t.Errorf("a new room is %v, want AVAILABLE", room["status"])
	}
	if status, _ := h.do(http.MethodPost, "/rooms", map[string]any{"room_type_id": rtID, "number": "101"}); status == http.StatusCreated {
		t.Error("two rooms numbered 101 were created")
	}

	// Booking.
	h.must(http.StatusBadRequest, "checkout before check-in", http.MethodPost, "/reservations",
		map[string]any{"room_type_id": rtID, "check_in_date": day(3), "check_out_date": day(1), "guests": 1})
	res := h.must(http.StatusCreated, "book", http.MethodPost, "/reservations",
		map[string]any{"room_type_id": rtID, "check_in_date": day(0), "check_out_date": day(2), "guests": 2})
	resID := idOf(t, res, "id")
	if res["status"] != "REQUESTED" {
		t.Errorf("a new reservation is %v, want REQUESTED", res["status"])
	}
	h.must(http.StatusConflict, "check in before confirming", http.MethodPost, "/reservations/"+resID+"/check-in", map[string]any{"room_id": roomID})
	h.must(http.StatusOK, "confirm", http.MethodPost, "/reservations/"+resID+"/confirm", nil)
	h.must(http.StatusConflict, "confirm twice", http.MethodPost, "/reservations/"+resID+"/confirm", nil)
	h.must(http.StatusBadRequest, "charge before check-in", http.MethodPost, "/reservations/"+resID+"/folio/charges", map[string]any{"description": "Minibar", "amount": 50})
	h.must(http.StatusBadRequest, "check in without a room", http.MethodPost, "/reservations/"+resID+"/check-in", map[string]any{})

	// Stay.
	in := h.must(http.StatusOK, "check in", http.MethodPost, "/reservations/"+resID+"/check-in", map[string]any{"room_id": roomID})
	if r, _ := in["reservation"].(map[string]any); r["status"] != "CHECKED_IN" {
		t.Errorf("after check-in the reservation is %v", r["status"])
	}
	if f, _ := in["folio"].(map[string]any); f["status"] != "OPEN" {
		t.Errorf("check-in opens a folio; got %v", in["folio"])
	}
	roomStatus := func() string {
		for _, r := range listOf(t, h.must(http.StatusOK, "list rooms", http.MethodGet, "/rooms", nil), "rooms") {
			if r["id"] == roomID {
				return r["status"].(string)
			}
		}
		return "MISSING"
	}
	if got := roomStatus(); got != "OCCUPIED" {
		t.Errorf("room after check-in = %s, want OCCUPIED", got)
	}
	h.must(http.StatusBadRequest, "negative charge", http.MethodPost, "/reservations/"+resID+"/folio/charges", map[string]any{"description": "Oops", "amount": -5})
	h.must(http.StatusBadRequest, "charge with no description", http.MethodPost, "/reservations/"+resID+"/folio/charges", map[string]any{"amount": 5})
	h.must(http.StatusCreated, "add a charge", http.MethodPost, "/reservations/"+resID+"/folio/charges", map[string]any{"description": "Minibar", "amount": 450})
	folio := h.must(http.StatusOK, "read the folio", http.MethodGet, "/reservations/"+resID+"/folio", nil)
	if !strings.Contains(folio["__raw"].(string), "Minibar") {
		t.Errorf("the folio does not list the charge: %v", folio["__raw"])
	}

	// Check-out hands the room to housekeeping and settles the folio.
	h.must(http.StatusOK, "check out", http.MethodPost, "/reservations/"+resID+"/check-out", nil)
	h.must(http.StatusConflict, "check out twice", http.MethodPost, "/reservations/"+resID+"/check-out", nil)
	if got := roomStatus(); got != "CHECKOUT_PENDING" {
		t.Errorf("room after check-out = %s, want CHECKOUT_PENDING", got)
	}
	folio = h.must(http.StatusOK, "folio after checkout", http.MethodGet, "/reservations/"+resID+"/folio", nil)
	if !strings.Contains(folio["__raw"].(string), "SETTLED") {
		t.Errorf("the folio is not SETTLED after check-out: %v", folio["__raw"])
	}

	// Housekeeping: the room goes CHECKOUT_PENDING → CLEANING → INSPECTION → AVAILABLE, no shortcuts.
	h.must(http.StatusConflict, "skip straight to available", http.MethodPost, "/rooms/"+roomID+"/status", map[string]any{"status": "AVAILABLE"})
	task := h.must(http.StatusCreated, "create a cleaning task", http.MethodPost, "/housekeeping", map[string]any{"room_id": roomID, "task_type": "CLEANING"})
	h.must(http.StatusBadRequest, "a task for a room that is not ours", http.MethodPost, "/housekeeping", map[string]any{"room_id": uuid.NewString(), "task_type": "CLEANING"})
	h.must(http.StatusBadRequest, "a task assigned to a stranger", http.MethodPost, "/housekeeping", map[string]any{"room_id": roomID, "assigned_to": uuid.NewString()})
	if n := len(listOf(t, h.must(http.StatusOK, "list open tasks", http.MethodGet, "/housekeeping", nil), "tasks")); n != 1 {
		t.Errorf("%d open housekeeping tasks, want 1", n)
	}
	for _, to := range []string{"CLEANING", "INSPECTION", "AVAILABLE"} {
		h.must(http.StatusOK, "room to "+to, http.MethodPost, "/rooms/"+roomID+"/status", map[string]any{"status": to})
	}
	h.must(http.StatusOK, "complete the task", http.MethodPost, "/housekeeping/"+idOf(t, task, "id")+"/complete", nil)
	if got := roomStatus(); got != "AVAILABLE" {
		t.Errorf("room at the end = %s, want AVAILABLE", got)
	}
	if n := len(listOf(t, h.must(http.StatusOK, "list tasks", http.MethodGet, "/housekeeping", nil), "tasks")); n != 0 {
		t.Errorf("%d open housekeeping tasks after completing it, want 0 (the list shows open work)", n)
	}
}

func TestHotelCancellationsAndNoShows(t *testing.T) {
	env := newTestEnv(t)
	h := newBiz(env, "HOTEL")
	rt := idOf(t, h.must(http.StatusCreated, "room type", http.MethodPost, "/room-types", map[string]any{"name": "Std", "base_price": 1000}), "id")
	book := func() string {
		return idOf(t, h.must(http.StatusCreated, "book", http.MethodPost, "/reservations",
			map[string]any{"room_type_id": rt, "check_in_date": time.Now().UTC().Format("2006-01-02"), "check_out_date": time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")}), "id")
	}
	a := book()
	h.must(http.StatusOK, "cancel a requested booking", http.MethodPost, "/reservations/"+a+"/cancel", nil)
	h.must(http.StatusConflict, "confirm a cancelled booking", http.MethodPost, "/reservations/"+a+"/confirm", nil)
	h.must(http.StatusConflict, "cancel twice", http.MethodPost, "/reservations/"+a+"/cancel", nil)

	b := book()
	h.must(http.StatusConflict, "no-show before confirming", http.MethodPost, "/reservations/"+b+"/no-show", nil)
	h.must(http.StatusOK, "confirm", http.MethodPost, "/reservations/"+b+"/confirm", nil)
	h.must(http.StatusOK, "no-show", http.MethodPost, "/reservations/"+b+"/no-show", nil)
	h.must(http.StatusConflict, "revive a no-show", http.MethodPost, "/reservations/"+b+"/confirm", nil)
}

func TestHotelDataNeverCrossesBusinesses(t *testing.T) {
	env := newTestEnv(t)
	a, b := newBiz(env, "HOTEL"), newBiz(env, "HOTEL")
	rt := idOf(t, a.must(http.StatusCreated, "A room type", http.MethodPost, "/room-types", map[string]any{"name": "Std", "base_price": 1}), "id")
	room := idOf(t, a.must(http.StatusCreated, "A room", http.MethodPost, "/rooms", map[string]any{"room_type_id": rt, "number": "1"}), "id")
	res := idOf(t, a.must(http.StatusCreated, "A booking", http.MethodPost, "/reservations",
		map[string]any{"room_type_id": rt, "check_in_date": time.Now().UTC().Format("2006-01-02"), "check_out_date": time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")}), "id")

	b.must(http.StatusNotFound, "B reads A's reservation", http.MethodGet, "/reservations/"+res, nil)
	b.must(http.StatusNotFound, "B confirms A's reservation", http.MethodPost, "/reservations/"+res+"/confirm", nil)
	b.must(http.StatusNotFound, "B changes A's room", http.MethodPost, "/rooms/"+room+"/status", map[string]any{"status": "RESERVED"})
	b.must(http.StatusBadRequest, "B books A's room type", http.MethodPost, "/reservations",
		map[string]any{"room_type_id": rt, "check_in_date": time.Now().UTC().Format("2006-01-02"), "check_out_date": time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")})
	if raw := b.must(http.StatusOK, "B lists reservations", http.MethodGet, "/reservations", nil)["__raw"].(string); strings.Contains(raw, res) {
		t.Error("B's reservation list contains A's booking")
	}
	// And a barber has no hotel module at all.
	barber := newBiz(env, "BARBER")
	if status, body := barber.do(http.MethodGet, "/reservations", nil); status != http.StatusForbidden || errCode(body) != "capability_disabled" {
		t.Errorf("a barber reaching /reservations = %d %s, want 403 capability_disabled", status, errCode(body))
	}
}

func TestBarberAppointmentThroughTheQueue(t *testing.T) {
	env := newTestEnv(t)
	b := newBiz(env, "BARBER")
	soon := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)

	b.must(http.StatusBadRequest, "service needs a name", http.MethodPost, "/services", map[string]any{"duration_minutes": 30, "price": 200})
	svc := idOf(t, b.must(http.StatusCreated, "create service", http.MethodPost, "/services", map[string]any{"name": "Haircut", "duration_minutes": 30, "price": 200}), "id")
	b.must(http.StatusOK, "edit service", http.MethodPatch, "/services/"+svc, map[string]any{"price": 250})
	if n := len(listOf(t, b.must(http.StatusOK, "list services", http.MethodGet, "/services", nil), "services")); n != 1 {
		t.Errorf("%d services, want 1", n)
	}

	b.must(http.StatusBadRequest, "appointment needs a time", http.MethodPost, "/appointments", map[string]any{"service_id": svc})
	b.must(http.StatusBadRequest, "appointment with a bad time", http.MethodPost, "/appointments", map[string]any{"service_id": svc, "scheduled_at": "tomorrow-ish"})
	b.must(http.StatusBadRequest, "appointment for an unknown service", http.MethodPost, "/appointments", map[string]any{"service_id": uuid.NewString(), "scheduled_at": soon})
	appt := idOf(t, b.must(http.StatusCreated, "book", http.MethodPost, "/appointments", map[string]any{"service_id": svc, "scheduled_at": soon}), "id")

	b.must(http.StatusConflict, "check in before confirming", http.MethodPost, "/appointments/"+appt+"/checkin", nil)
	b.must(http.StatusOK, "confirm", http.MethodPost, "/appointments/"+appt+"/confirm", nil)
	b.must(http.StatusConflict, "confirm twice", http.MethodPost, "/appointments/"+appt+"/confirm", nil)

	// Check-in puts the customer in the queue, WAITING.
	in := b.must(http.StatusOK, "check in", http.MethodPost, "/appointments/"+appt+"/checkin", nil)
	entry, _ := in["queue_entry"].(map[string]any)
	if a, _ := in["appointment"].(map[string]any); a["status"] != "WAITING" || entry["status"] != "WAITING" {
		t.Errorf("after check-in: appointment %v, queue %v: both want WAITING", in["appointment"], entry["status"])
	}
	entryID := idOf(t, entry, "id")
	b.must(http.StatusConflict, "start before calling", http.MethodPost, "/queue/"+entryID+"/start", nil)
	b.must(http.StatusOK, "call", http.MethodPost, "/queue/"+entryID+"/call", nil)
	b.must(http.StatusOK, "start", http.MethodPost, "/queue/"+entryID+"/start", nil)
	got := b.must(http.StatusOK, "read the appointment", http.MethodGet, "/appointments/"+appt, nil)
	if got["status"] != "IN_SERVICE" {
		t.Errorf("starting the queue entry moves the appointment to %v, want IN_SERVICE", got["status"])
	}
	b.must(http.StatusOK, "complete", http.MethodPost, "/queue/"+entryID+"/complete", nil)
	if got = b.must(http.StatusOK, "read it again", http.MethodGet, "/appointments/"+appt, nil); got["status"] != "COMPLETED" {
		t.Errorf("completing the queue entry moves the appointment to %v, want COMPLETED", got["status"])
	}
	b.must(http.StatusConflict, "cancel a completed appointment", http.MethodPost, "/appointments/"+appt+"/cancel", nil)
}

func TestBarberWalkInsCancellationsAndNoShows(t *testing.T) {
	env := newTestEnv(t)
	b := newBiz(env, "BARBER")
	svc := idOf(t, b.must(http.StatusCreated, "service", http.MethodPost, "/services", map[string]any{"name": "Shave", "duration_minutes": 15, "price": 100}), "id")
	soon := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// Walk-ins get consecutive ticket numbers and can leave the queue.
	w1 := b.must(http.StatusCreated, "walk-in 1", http.MethodPost, "/queue", map[string]any{})
	w2 := b.must(http.StatusCreated, "walk-in 2", http.MethodPost, "/queue", map[string]any{})
	if w2["queue_number"].(float64) != w1["queue_number"].(float64)+1 {
		t.Errorf("walk-in ticket numbers %v then %v are not consecutive", w1["queue_number"], w2["queue_number"])
	}
	b.must(http.StatusOK, "walk-in 1 leaves", http.MethodPost, "/queue/"+idOf(t, w1, "id")+"/cancel", nil)
	b.must(http.StatusConflict, "call a cancelled ticket", http.MethodPost, "/queue/"+idOf(t, w1, "id")+"/call", nil)
	if n := len(listOf(t, b.must(http.StatusOK, "list the queue", http.MethodGet, "/queue", nil), "queue")); n != 2 {
		t.Errorf("the queue shows %d entries, want 2 (cancelled ones stay visible)", n)
	}

	// Cancel and no-show.
	a := idOf(t, b.must(http.StatusCreated, "book", http.MethodPost, "/appointments", map[string]any{"service_id": svc, "scheduled_at": soon}), "id")
	b.must(http.StatusOK, "cancel while requested", http.MethodPost, "/appointments/"+a+"/cancel", nil)
	b.must(http.StatusConflict, "confirm a cancelled appointment", http.MethodPost, "/appointments/"+a+"/confirm", nil)
	c := idOf(t, b.must(http.StatusCreated, "book", http.MethodPost, "/appointments", map[string]any{"service_id": svc, "scheduled_at": soon}), "id")
	b.must(http.StatusConflict, "no-show before confirming", http.MethodPost, "/appointments/"+c+"/no-show", nil)
	b.must(http.StatusOK, "confirm", http.MethodPost, "/appointments/"+c+"/confirm", nil)
	b.must(http.StatusOK, "no-show", http.MethodPost, "/appointments/"+c+"/no-show", nil)

	// Service lifecycle: one that has appointments cannot be deleted (it must not
	// be a 500), but a fresh one can.
	if status, body := b.do(http.MethodDelete, "/services/"+svc, nil); status != http.StatusConflict || errCode(body) != "in_use" {
		t.Errorf("deleting a service with appointments = %d %s, want 409 in_use", status, errCode(body))
	}
	spare := idOf(t, b.must(http.StatusCreated, "spare service", http.MethodPost, "/services", map[string]any{"name": "Spare", "duration_minutes": 5, "price": 1}), "id")
	b.must(http.StatusOK, "delete an unused service", http.MethodDelete, "/services/"+spare, nil)
}

func TestBarberDataNeverCrossesBusinesses(t *testing.T) {
	env := newTestEnv(t)
	a, b := newBiz(env, "BARBER"), newBiz(env, "BARBER")
	svc := idOf(t, a.must(http.StatusCreated, "A service", http.MethodPost, "/services", map[string]any{"name": "Cut", "duration_minutes": 20, "price": 10}), "id")
	appt := idOf(t, a.must(http.StatusCreated, "A booking", http.MethodPost, "/appointments",
		map[string]any{"service_id": svc, "scheduled_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}), "id")
	q := idOf(t, a.must(http.StatusCreated, "A walk-in", http.MethodPost, "/queue", map[string]any{}), "id")

	b.must(http.StatusNotFound, "B reads A's appointment", http.MethodGet, "/appointments/"+appt, nil)
	b.must(http.StatusNotFound, "B confirms A's appointment", http.MethodPost, "/appointments/"+appt+"/confirm", nil)
	b.must(http.StatusNotFound, "B calls A's ticket", http.MethodPost, "/queue/"+q+"/call", nil)
	b.must(http.StatusBadRequest, "B books A's service", http.MethodPost, "/appointments",
		map[string]any{"service_id": svc, "scheduled_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if raw := b.must(http.StatusOK, "B lists services", http.MethodGet, "/services", nil)["__raw"].(string); strings.Contains(raw, svc) {
		t.Error("B's service list contains A's service")
	}
}

func TestRestaurantTablesLifecycle(t *testing.T) {
	env := newTestEnv(t)
	r := newBiz(env, "RESTAURANT")
	other := newBiz(env, "RESTAURANT")

	r.must(http.StatusBadRequest, "a table needs a label", http.MethodPost, "/tables", map[string]any{"seats": 4})
	tb := r.must(http.StatusCreated, "create a table", http.MethodPost, "/tables", map[string]any{"label": "T1", "seats": 4})
	id := idOf(t, tb, "id")
	if tb["status"] != "AVAILABLE" {
		t.Errorf("a new table is %v, want AVAILABLE", tb["status"])
	}
	if up := r.must(http.StatusOK, "edit seats", http.MethodPatch, "/tables/"+id, map[string]any{"seats": 6}); up["seats"] != float64(6) {
		t.Errorf("seats after edit = %v, want 6", up["seats"])
	}
	// Only the moves the contract allows: seat guests, then clean, then free it.
	r.must(http.StatusConflict, "free a table that is already free", http.MethodPost, "/tables/"+id+"/status", map[string]any{"status": "AVAILABLE"})
	r.must(http.StatusConflict, "clean a table nobody used", http.MethodPost, "/tables/"+id+"/status", map[string]any{"status": "CLEANING"})
	r.must(http.StatusConflict, "an unknown status", http.MethodPost, "/tables/"+id+"/status", map[string]any{"status": "ON_FIRE"})
	for _, to := range []string{"OCCUPIED", "CLEANING", "AVAILABLE"} {
		r.must(http.StatusOK, "table to "+to, http.MethodPost, "/tables/"+id+"/status", map[string]any{"status": to})
	}
	if n := len(listOf(t, r.must(http.StatusOK, "list tables", http.MethodGet, "/tables", nil), "tables")); n != 1 {
		t.Errorf("%d tables, want 1", n)
	}

	// Another business sees none of it and can change none of it.
	if n := len(listOf(t, other.must(http.StatusOK, "other lists tables", http.MethodGet, "/tables", nil), "tables")); n != 0 {
		t.Errorf("another business sees %d of this business's tables", n)
	}
	other.must(http.StatusNotFound, "other moves our table", http.MethodPost, "/tables/"+id+"/status", map[string]any{"status": "OCCUPIED"})
	other.must(http.StatusNotFound, "other edits our table", http.MethodPatch, "/tables/"+id, map[string]any{"seats": 99})

	r.must(http.StatusOK, "delete", http.MethodDelete, "/tables/"+id, nil)
	if n := len(listOf(t, r.must(http.StatusOK, "list after delete", http.MethodGet, "/tables", nil), "tables")); n != 0 {
		t.Errorf("%d tables after deleting the only one", n)
	}
}
