package httpserver

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Storefront booking: a customer books time with a barber or a room at a
// hotel from the public shop, signed in. These walk the real flows through
// the API, including the races and the walls between businesses.

// bookable sets up a published booking business with one service and one
// staff member who works every day, and returns it with its public host.
func bookable(t *testing.T, env *testEnv, kind string) (biz, string, string) {
	t.Helper()
	b := newBiz(env, kind)
	env.publishTenant(b.id)
	svc := b.must(http.StatusCreated, "create service", http.MethodPost, "/services",
		map[string]any{"name": "Haircut", "duration_minutes": 30, "price": 250, "is_active": true})
	var staffID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO users (tenant_id, name, email, password_hash, role, status)
		 VALUES ($1, 'Ravi', $2, 'x', 'STAFF', 'ACTIVE') RETURNING id`,
		b.id, "ravi-"+randSuffix()+"@test.local").Scan(&staffID); err != nil {
		t.Fatalf("create staff: %v", err)
	}
	for wd := 0; wd < 7; wd++ {
		b.must(http.StatusCreated, "set hours", http.MethodPost, "/staff-availability", map[string]any{
			"staff_id": staffID.String(), "weekday": wd, "start_time": "09:00", "end_time": "18:00",
		})
	}
	return b, env.hostFor(b.id), idOf(t, svc, "id")
}

func signIn(t *testing.T, env *testEnv, host string) string {
	t.Helper()
	return env.verifyOTP(t, &shopFixture{host: host}, "98"+strconv.Itoa(10000000+int(time.Now().UnixNano()%89999999)))
}

func slotsOn(t *testing.T, env *testEnv, host, serviceID string, days int) []map[string]any {
	t.Helper()
	date := time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
	status, body := env.doOn(http.MethodGet, "/api/v1/public/slots?service_id="+serviceID+"&date="+date, "", nil, host)
	env.mustStatus(http.StatusOK, status, "list slots", body)
	slots := listOf(t, body, "slots")
	if len(slots) < 4 {
		t.Fatalf("want a working day of slots, got %d", len(slots))
	}
	return slots
}

func TestStorefrontAppointmentBooking(t *testing.T) {
	env := newTestEnv(t)
	b, host, serviceID := bookable(t, env, "BARBER")

	status, store := env.doOn(http.MethodGet, "/api/v1/public/store", "", nil, host)
	env.mustStatus(http.StatusOK, status, "store config", store)
	if _, ok := store["booking"].(map[string]any); !ok {
		t.Errorf("store config has no booking settings: %v", store["__raw"])
	}
	if caps, _ := store["capabilities"].([]any); len(caps) == 0 {
		t.Errorf("store config has no capabilities: %v", store["__raw"])
	}

	status, body := env.doOn(http.MethodGet, "/api/v1/public/services", "", nil, host)
	env.mustStatus(http.StatusOK, status, "list services", body)
	if len(listOf(t, body, "services")) != 1 || len(listOf(t, body, "staff")) != 1 {
		t.Fatalf("want one service and one staff member: %v", body["__raw"])
	}

	slots := slotsOn(t, env, host, serviceID, 2)
	book := func(token, start, clientToken string) (int, map[string]any) {
		return env.doOn(http.MethodPost, "/api/v1/public/appointments", token, map[string]any{
			"service_id": serviceID, "staff_id": "", "start": start, "notes": "", "client_token": clientToken,
		}, host)
	}

	// Booking needs a signed-in customer.
	status, body = book("", slots[0]["start"].(string), "anon")
	env.mustStatus(http.StatusUnauthorized, status, "book signed out", body)

	alice := signIn(t, env, host)
	status, first := book(alice, slots[0]["start"].(string), "alice-1")
	env.mustStatus(http.StatusCreated, status, "book", first)
	firstID := idOf(t, first, "id")
	if first["status"] != "REQUESTED" || first["service_name"] != "Haircut" {
		t.Errorf("unexpected booking: %v", first["__raw"])
	}

	// A double tap returns the same booking.
	status, again := book(alice, slots[0]["start"].(string), "alice-1")
	env.mustStatus(http.StatusOK, status, "repeat booking", again)
	if again["id"] != firstID {
		t.Errorf("a repeated client token made a second booking")
	}

	// The time is gone for everyone else.
	bob := signIn(t, env, host)
	status, body = book(bob, slots[0]["start"].(string), "bob-1")
	env.mustStatus(http.StatusConflict, status, "book a taken time", body)

	// Ten customers race for one time: exactly one gets it.
	race := slots[2]["start"].(string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := env.doOn(http.MethodPost, "/api/v1/public/appointments", bob, map[string]any{
				"service_id": serviceID, "staff_id": "", "start": race, "notes": "", "client_token": "race-" + strconv.Itoa(i),
			}, host)
			if code == http.StatusCreated {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d customers booked the same time, want exactly 1", won)
	}

	// The business sees where the booking came from.
	staffView := b.must(http.StatusOK, "staff list", http.MethodGet, "/appointments", nil)
	online := 0
	for _, a := range listOf(t, staffView, "appointments") {
		if a["source"] == "ONLINE" {
			online++
		}
	}
	if online != 2 {
		t.Errorf("staff see %d online bookings, want 2", online)
	}

	// The customer sees only their own.
	status, mine := env.doOn(http.MethodGet, "/api/v1/customer/appointments", alice, nil, host)
	env.mustStatus(http.StatusOK, status, "my bookings", mine)
	if got := len(listOf(t, mine, "appointments")); got != 1 {
		t.Errorf("alice sees %d bookings, want 1", got)
	}

	// Move it, then cancel it. Someone else cannot touch it.
	status, body = env.doOn(http.MethodPost, "/api/v1/customer/appointments/"+firstID+"/cancel", bob, nil, host)
	env.mustStatus(http.StatusNotFound, status, "cancel someone else's booking", body)
	moveTo := slots[4]["start"].(string)
	status, moved := env.doOn(http.MethodPost, "/api/v1/customer/appointments/"+firstID+"/reschedule", alice,
		map[string]any{"start": moveTo, "staff_id": ""}, host)
	env.mustStatus(http.StatusOK, status, "reschedule", moved)
	if !sameInstant(moved["scheduled_at"], moveTo) {
		t.Errorf("rescheduled to %v, want %s", moved["scheduled_at"], moveTo)
	}
	status, body = book(bob, slots[0]["start"].(string), "bob-2")
	env.mustStatus(http.StatusCreated, status, "the old time is free again", body)

	status, cancelled := env.doOn(http.MethodPost, "/api/v1/customer/appointments/"+firstID+"/cancel", alice, nil, host)
	env.mustStatus(http.StatusOK, status, "cancel", cancelled)
	if cancelled["status"] != "CANCELLED" {
		t.Errorf("after cancel the booking is %v", cancelled["status"])
	}
	status, body = env.doOn(http.MethodPost, "/api/v1/customer/appointments/"+firstID+"/cancel", alice, nil, host)
	env.mustStatus(http.StatusConflict, status, "cancel twice", body)
}

func sameInstant(a any, b string) bool {
	s, _ := a.(string)
	x, err1 := time.Parse(time.RFC3339, s)
	y, err2 := time.Parse(time.RFC3339, b)
	return err1 == nil && err2 == nil && x.Equal(y)
}

func TestStorefrontBookingStaysInsideItsBusiness(t *testing.T) {
	env := newTestEnv(t)
	_, hostA, _ := bookable(t, env, "BARBER")
	_, hostB, serviceB := bookable(t, env, "SALON")

	tokenA := signIn(t, env, hostA)
	slotsB := slotsOn(t, env, hostB, serviceB, 2)

	// A customer of shop A is not signed in at shop B.
	status, body := env.doOn(http.MethodPost, "/api/v1/public/appointments", tokenA, map[string]any{
		"service_id": serviceB, "staff_id": "", "start": slotsB[0]["start"], "notes": "", "client_token": "x",
	}, hostB)
	env.mustStatus(http.StatusUnauthorized, status, "book at another shop with this shop's token", body)

	// Shop B's service cannot be booked on shop A's host.
	status, body = env.doOn(http.MethodPost, "/api/v1/public/appointments", tokenA, map[string]any{
		"service_id": serviceB, "staff_id": "", "start": slotsB[0]["start"], "notes": "", "client_token": "y",
	}, hostA)
	env.mustStatus(http.StatusBadRequest, status, "book another shop's service", body)

	// A food shop has no booking at all, and a restaurant has no rooms.
	food := newShop(t, env)
	status, body = food.guest(http.MethodGet, "/api/v1/public/services", nil)
	env.mustStatus(http.StatusForbidden, status, "services at a food shop", body)
	rest := newBiz(env, "RESTAURANT")
	env.publishTenant(rest.id)
	status, body = env.doOn(http.MethodPost, "/api/v1/public/reservations", "", map[string]any{}, env.hostFor(rest.id))
	env.mustStatus(http.StatusForbidden, status, "reserve a room at a restaurant", body)
}

func TestStorefrontHotelStay(t *testing.T) {
	env := newTestEnv(t)
	h := newBiz(env, "HOTEL")
	env.publishTenant(h.id)
	host := env.hostFor(h.id)
	rt := h.must(http.StatusCreated, "room type", http.MethodPost, "/room-types",
		map[string]any{"name": "Deluxe", "base_price": 3000, "max_guests": 2, "description": "Sea view"})
	rtID := idOf(t, rt, "id")
	if rt["description"] != "Sea view" {
		t.Errorf("room type description not saved: %v", rt["__raw"])
	}
	h.must(http.StatusCreated, "room", http.MethodPost, "/rooms", map[string]any{"room_type_id": rtID, "number": "201"})

	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }
	search := func(in, out string, guests int) map[string]any {
		status, body := env.doOn(http.MethodGet, "/api/v1/public/room-types?check_in="+in+"&check_out="+out+"&guests="+strconv.Itoa(guests), "", nil, host)
		env.mustStatus(http.StatusOK, status, "search rooms", body)
		return listOf(t, body, "room_types")[0]
	}
	found := search(day(5), day(7), 2)
	if found["available"] != float64(1) || found["nights"] != float64(2) || found["total"] != float64(6000) {
		t.Fatalf("unexpected availability: %v", found)
	}
	status, body := env.doOn(http.MethodGet, "/api/v1/public/room-types?check_in="+day(7)+"&check_out="+day(5), "", nil, host)
	env.mustStatus(http.StatusBadRequest, status, "reversed dates", body)

	guest := signIn(t, env, host)
	reserve := func(token, in, out string, guests int, ct string) (int, map[string]any) {
		return env.doOn(http.MethodPost, "/api/v1/public/reservations", token, map[string]any{
			"room_type_id": rtID, "check_in": in, "check_out": out, "guests": guests, "notes": "", "client_token": ct,
		}, host)
	}
	status, body = reserve(guest, day(5), day(7), 3, "too-many")
	env.mustStatus(http.StatusBadRequest, status, "more guests than the room fits", body)

	// Two guests race for the only room.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = reserve(guest, day(5), day(7), 2, "race-"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()
	if !((codes[0] == http.StatusCreated && codes[1] == http.StatusConflict) || (codes[1] == http.StatusCreated && codes[0] == http.StatusConflict)) {
		t.Fatalf("racing for the last room gave %v, want one 201 and one 409", codes)
	}
	if search(day(6), day(7), 2)["available"] != float64(0) {
		t.Error("an overlapping night still shows the room as free")
	}

	// Checking out on the day the next guest checks in shares the room.
	status, next := reserve(guest, day(7), day(8), 2, "next")
	env.mustStatus(http.StatusCreated, status, "back-to-back stay", next)
	if next["total_amount"] != float64(3000) {
		t.Errorf("one night costs %v, want 3000", next["total_amount"])
	}

	status, mine := env.doOn(http.MethodGet, "/api/v1/customer/reservations", guest, nil, host)
	env.mustStatus(http.StatusOK, status, "my stays", mine)
	if got := len(listOf(t, mine, "reservations")); got != 2 {
		t.Fatalf("guest sees %d stays, want 2", got)
	}
	nextID := idOf(t, next, "id")
	stranger := signIn(t, env, host)
	status, body = env.doOn(http.MethodPost, "/api/v1/customer/reservations/"+nextID+"/cancel", stranger, nil, host)
	env.mustStatus(http.StatusNotFound, status, "cancel someone else's stay", body)
	status, body = env.doOn(http.MethodPost, "/api/v1/customer/reservations/"+nextID+"/cancel", guest, nil, host)
	env.mustStatus(http.StatusOK, status, "cancel", body)
	if search(day(7), day(8), 2)["available"] != float64(1) {
		t.Error("a cancelled stay still holds the room")
	}
}

func TestNewBusinessTypesGetTheirCapabilities(t *testing.T) {
	env := newTestEnv(t)
	has := func(id uuid.UUID, code string) bool {
		var on bool
		_ = env.server.pool.QueryRow(tctx(),
			`SELECT enabled FROM tenant_capabilities WHERE tenant_id = $1 AND capability_code = $2`, id, code).Scan(&on)
		return on
	}
	for kind, code := range map[string]string{
		"SALON": "APPOINTMENTS", "AUTOMOTIVE": "APPOINTMENTS", "FASHION": "CATALOG", "ELECTRONICS": "ORDERS",
	} {
		id := env.createTenantOfType("Type "+kind, "type-"+randSuffix(), kind)
		if !has(id, code) {
			t.Errorf("%s does not have %s", kind, code)
		}
	}
}
