package httpserver

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/appointments"
	"github.com/orderly/orderly-backend/internal/hotel"
	"github.com/orderly/orderly-backend/internal/orders"
	"github.com/orderly/orderly-backend/internal/tables"
)

// Commerce & operations contract freeze (Release V1 §3.4, ticket T2).
//
// The state machines below are the frozen contract documented in
// book/LLD.md. Changing one is a deliberate act: edit the golden table here,
// the LLD and the DB CHECK constraint in the same change. These tests make
// drift between those three places impossible to miss.

type machine struct {
	name        string
	table       string // DB table whose status CHECK must list exactly these states
	transitions map[string][]string
	can         func(from, to string) bool
}

var frozenMachines = []machine{
	{"order", "orders", map[string][]string{
		"PENDING": {"ACCEPTED", "CANCELLED"}, "ACCEPTED": {"PREPARING", "CANCELLED"},
		"PREPARING": {"READY"}, "READY": {"COMPLETED"}, "COMPLETED": {}, "CANCELLED": {},
	}, orders.CanTransition},
	{"appointment", "appointments", map[string][]string{
		"REQUESTED": {"CONFIRMED", "CANCELLED"}, "CONFIRMED": {"CHECKED_IN", "CANCELLED", "NO_SHOW"},
		"CHECKED_IN": {"WAITING"}, "WAITING": {"IN_SERVICE"}, "IN_SERVICE": {"COMPLETED"},
		"COMPLETED": {}, "CANCELLED": {}, "NO_SHOW": {},
	}, appointments.CanTransition},
	{"queue entry", "queue_entries", map[string][]string{
		"WAITING": {"CALLED", "CANCELLED"}, "CALLED": {"IN_SERVICE", "CANCELLED"},
		"IN_SERVICE": {"COMPLETED"}, "COMPLETED": {}, "CANCELLED": {},
	}, appointments.CanTransitionQueue},
	{"reservation", "reservations", map[string][]string{
		"REQUESTED": {"CONFIRMED", "CANCELLED"}, "CONFIRMED": {"CHECKED_IN", "CANCELLED", "NO_SHOW"},
		"CHECKED_IN": {"CHECKED_OUT"}, "CHECKED_OUT": {}, "CANCELLED": {}, "NO_SHOW": {},
	}, hotel.CanTransitionReservation},
	{"room", "rooms", map[string][]string{
		"AVAILABLE": {"RESERVED", "OCCUPIED"}, "RESERVED": {"OCCUPIED", "AVAILABLE"},
		"OCCUPIED": {"CHECKOUT_PENDING"}, "CHECKOUT_PENDING": {"CLEANING"},
		"CLEANING": {"INSPECTION"}, "INSPECTION": {"AVAILABLE"},
	}, hotel.CanTransitionRoom},
	{"table", "tables", map[string][]string{
		"AVAILABLE": {"OCCUPIED", "RESERVED"}, "RESERVED": {"OCCUPIED", "AVAILABLE"},
		"OCCUPIED": {"CLEANING"}, "CLEANING": {"AVAILABLE"},
	}, tables.CanTransition},
}

// TestStateMachinesMatchTheFrozenContract: the code allows exactly the moves
// in the golden tables — no more, no fewer.
func TestStateMachinesMatchTheFrozenContract(t *testing.T) {
	for _, m := range frozenMachines {
		states := sortedKeys(m.transitions)
		for _, from := range states {
			allowed := map[string]bool{}
			for _, to := range m.transitions[from] {
				allowed[to] = true
			}
			for _, to := range states {
				if got := m.can(from, to); got != allowed[to] {
					t.Errorf("%s %s → %s: code allows = %v, frozen contract says %v", m.name, from, to, got, allowed[to])
				}
			}
			if m.can(from, "NOT_A_STATE") {
				t.Errorf("%s: %s → an unknown state must never be legal", m.name, from)
			}
		}
		if m.can("NOT_A_STATE", states[0]) {
			t.Errorf("%s: a move from an unknown state must never be legal", m.name)
		}
	}
}

// TestStateMachinesMatchTheDatabase: the states the code knows are exactly
// the states the table's CHECK constraint accepts. A state added in only one
// place would be a runtime 500 (or a dead state) waiting to happen.
func TestStateMachinesMatchTheDatabase(t *testing.T) {
	env := newTestEnv(t)
	quoted := regexp.MustCompile(`'([A-Z_]+)'`)
	for _, m := range frozenMachines {
		rows, err := env.server.pool.Query(tctx(), `
			SELECT pg_get_constraintdef(c.oid)
			FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
			WHERE t.relname = $1 AND c.contype = 'c'
			  AND pg_get_constraintdef(c.oid) ~ '^CHECK \(\(?status = ANY|^CHECK \(\(?status IN|status = ANY'`, m.table)
		if err != nil {
			t.Fatalf("%s: read constraints: %v", m.name, err)
		}
		db := map[string]bool{}
		for rows.Next() {
			var def string
			if err := rows.Scan(&def); err != nil {
				t.Fatal(err)
			}
			for _, match := range quoted.FindAllStringSubmatch(def, -1) {
				db[match[1]] = true
			}
		}
		rows.Close()
		if len(db) == 0 {
			t.Fatalf("%s: found no status CHECK constraint on %s", m.name, m.table)
		}
		for s := range m.transitions {
			if !db[s] {
				t.Errorf("%s: state %s is in the code but not in the %s.status CHECK", m.name, s, m.table)
			}
		}
		for s := range db {
			if _, ok := m.transitions[s]; !ok {
				t.Errorf("%s: state %s is allowed by the database but unknown to the code", m.name, s)
			}
		}
	}
}

// TestNoNewPerVerticalWorkflowPackages (C2): a state machine lives in one of
// the four packages the contract already covers. A new `CanTransition…` in any
// other package is a new fork of commerce/operations — the thing V1 forbids.
// Extend the shared contract instead (book/LLD.md → Commerce & Operations).
func TestNoNewPerVerticalWorkflowPackages(t *testing.T) {
	approved := map[string]bool{"orders": true, "appointments": true, "hotel": true, "tables": true}
	fn := regexp.MustCompile(`(?m)^func [Cc]anTransition\w*\(`)
	found := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if fn.Match(src) {
			found[filepath.Base(filepath.Dir(path))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for pkg := range found {
		if !approved[pkg] {
			t.Errorf("internal/%s defines its own state machine. V1 freezes commerce/operations contracts: "+
				"add to the shared contract in book/LLD.md and this file's frozen tables, or get the freeze lifted explicitly", pkg)
		}
	}
	for pkg := range approved {
		if !found[pkg] {
			t.Errorf("internal/%s no longer defines CanTransition…; update the contract guard if it moved", pkg)
		}
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// race fires n calls at once and tallies the HTTP statuses they return.
func race(n int, call func(i int) int) map[int]int {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		count = map[int]int{}
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			status := call(i)
			mu.Lock()
			count[status]++
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	return count
}

// wantOneWinner: exactly one 200, every loser a clean 409 — never a 500.
func wantOneWinner(t *testing.T, label string, got map[int]int, n int) {
	t.Helper()
	if got[http.StatusOK] != 1 || got[http.StatusConflict] != n-1 {
		t.Errorf("%s: statuses = %v, want one 200 and %d × 409", label, got, n-1)
	}
}

// TestOrderMovesAreAtomic: concurrent staff actions on one order cannot both
// win, and an illegal state is never reached (e.g. cancelling a PREPARING order).
func TestOrderMovesAreAtomic(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	place := func() (string, int) {
		o := shop.order(t, "Asha Rao", "9876543210", "CASH")
		n := number(t, o, "order_number")
		return env.orderID(shop.tenantID, n), n
	}
	post := func(id, action string) int {
		status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+id+"/"+action, nil)
		return status
	}

	// Ten simultaneous "accept" taps: one accept, one history row.
	id, _ := place()
	wantOneWinner(t, "10 × accept", race(10, func(int) int { return post(id, "accept") }), 10)
	if got := env.historyLength(id); got != 2 {
		t.Errorf("history after racing accepts = %d rows, want 2 (placed + accepted)", got)
	}

	// Cancel racing prepare on an ACCEPTED order: exactly one of them lands.
	id, _ = place()
	post(id, "accept")
	got := race(12, func(i int) int {
		if i%2 == 0 {
			return post(id, "prepare")
		}
		return post(id, "cancel")
	})
	wantOneWinner(t, "prepare vs cancel", got, 12)
	var status string
	if err := env.server.pool.QueryRow(tctx(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "PREPARING" && status != "CANCELLED" {
		t.Errorf("order ended %s, want PREPARING or CANCELLED", status)
	}
	if got := env.historyLength(id); got != 3 {
		t.Errorf("history = %d rows, want 3 (placed, accepted, one winner)", got)
	}
}

// TestAppointmentAndQueueMovesAreAtomic: a double tap on Check in queues the
// customer once, and two front-desk screens cannot both call the same ticket.
func TestAppointmentAndQueueMovesAreAtomic(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenantOfType("Race Barber", "race-barber-"+randSuffix(), "BARBER")
	admin := env.tenantAdmin(tenantID)
	var serviceID, apptID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO services (tenant_id, name, duration_minutes, price) VALUES ($1,'Cut',30,200) RETURNING id`,
		tenantID).Scan(&serviceID); err != nil {
		t.Fatal(err)
	}
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO appointments (tenant_id, service_id, scheduled_at, status)
		 VALUES ($1,$2, now() + interval '1 hour', 'CONFIRMED') RETURNING id`,
		tenantID, serviceID).Scan(&apptID); err != nil {
		t.Fatal(err)
	}
	post := func(path string) int {
		status, _ := env.doAs(http.MethodPost, path, admin, nil, tenantID)
		return status
	}

	wantOneWinner(t, "8 × check in",
		race(8, func(int) int { return post("/api/v1/tenant/appointments/" + apptID.String() + "/checkin") }), 8)
	var entries int
	var apptStatus, entryStatus, entryID string
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT count(*), max(status), max(id::text) FROM queue_entries WHERE appointment_id = $1`, apptID).
		Scan(&entries, &entryStatus, &entryID); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("racing check-ins queued the customer %d times, want 1", entries)
	}
	if err := env.server.pool.QueryRow(tctx(), `SELECT status FROM appointments WHERE id = $1`, apptID).Scan(&apptStatus); err != nil {
		t.Fatal(err)
	}
	if apptStatus != "WAITING" {
		t.Errorf("appointment status = %s, want WAITING", apptStatus)
	}
	wantOneWinner(t, "6 × call ticket",
		race(6, func(int) int { return post("/api/v1/tenant/queue/" + entryID + "/call") }), 6)
}

// TestHotelCheckInAndOutAreAtomic: two guests cannot be checked into the same
// room, a refused check-in leaves nothing half-done, and check-out is exactly-once.
func TestHotelCheckInAndOutAreAtomic(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenantOfType("Race Hotel", "race-hotel-"+randSuffix(), "HOTEL")
	admin := env.tenantAdmin(tenantID)
	var typeID, roomID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO room_types (tenant_id, name, base_price) VALUES ($1,'Std',1000) RETURNING id`,
		tenantID).Scan(&typeID); err != nil {
		t.Fatal(err)
	}
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO rooms (tenant_id, room_type_id, number) VALUES ($1,$2,'101') RETURNING id`,
		tenantID, typeID).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	reserve := func() uuid.UUID {
		var id uuid.UUID
		if err := env.server.pool.QueryRow(tctx(),
			`INSERT INTO reservations (tenant_id, room_type_id, check_in_date, check_out_date, status)
			 VALUES ($1,$2, CURRENT_DATE, CURRENT_DATE + 1, 'CONFIRMED') RETURNING id`,
			tenantID, typeID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	resA, resB := reserve(), reserve()
	checkIn := func(res uuid.UUID) int {
		status, _ := env.doAs(http.MethodPost, "/api/v1/tenant/reservations/"+res.String()+"/check-in",
			admin, map[string]any{"room_id": roomID.String()}, tenantID)
		return status
	}

	// Two desks, two guests, one room, six attempts at once.
	got := race(6, func(i int) int {
		if i%2 == 0 {
			return checkIn(resA)
		}
		return checkIn(resB)
	})
	wantOneWinner(t, "double-booking the room", got, 6)
	var checkedIn, folios int
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT count(*) FILTER (WHERE status = 'CHECKED_IN'), (SELECT count(*) FROM folios WHERE tenant_id = $1)
		 FROM reservations WHERE tenant_id = $1`, tenantID).Scan(&checkedIn, &folios); err != nil {
		t.Fatal(err)
	}
	if checkedIn != 1 || folios != 1 {
		t.Errorf("after the race: %d reservations checked in and %d folios, want 1 and 1", checkedIn, folios)
	}
	var roomStatus string
	_ = env.server.pool.QueryRow(tctx(), `SELECT status FROM rooms WHERE id = $1`, roomID).Scan(&roomStatus)
	if roomStatus != "OCCUPIED" {
		t.Errorf("room status = %s, want OCCUPIED", roomStatus)
	}

	// The loser's reservation must be untouched (no half check-in).
	loser := resA
	var loserStatus string
	_ = env.server.pool.QueryRow(tctx(), `SELECT status FROM reservations WHERE id = $1`, resA).Scan(&loserStatus)
	if loserStatus == "CHECKED_IN" {
		loser = resB
	}
	_ = env.server.pool.QueryRow(tctx(), `SELECT status FROM reservations WHERE id = $1`, loser).Scan(&loserStatus)
	if loserStatus != "CONFIRMED" {
		t.Errorf("the refused guest's reservation is %s, want it left CONFIRMED", loserStatus)
	}

	// Check-out is exactly-once, and settles the folio and releases the room.
	winner := resA
	if loser == resA {
		winner = resB
	}
	out := race(6, func(int) int {
		status, _ := env.doAs(http.MethodPost, "/api/v1/tenant/reservations/"+winner.String()+"/check-out", admin, nil, tenantID)
		return status
	})
	wantOneWinner(t, "6 × check out", out, 6)
	_ = env.server.pool.QueryRow(tctx(), `SELECT status FROM rooms WHERE id = $1`, roomID).Scan(&roomStatus)
	var folioStatus string
	_ = env.server.pool.QueryRow(tctx(), `SELECT status FROM folios WHERE tenant_id = $1`, tenantID).Scan(&folioStatus)
	if roomStatus != "CHECKOUT_PENDING" || folioStatus != "SETTLED" {
		t.Errorf("after check-out: room %s, folio %s; want CHECKOUT_PENDING and SETTLED", roomStatus, folioStatus)
	}
}

// TestTableStatusMovesAreAtomic: two screens flipping the same table cannot both succeed.
func TestTableStatusMovesAreAtomic(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Race Tables", "race-tables-"+randSuffix())
	admin := env.tenantAdmin(tenantID)
	var tableID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO tables (tenant_id, label) VALUES ($1,'T1') RETURNING id`, tenantID).Scan(&tableID); err != nil {
		t.Fatal(err)
	}
	got := race(8, func(int) int {
		status, _ := env.doAs(http.MethodPost, fmt.Sprintf("/api/v1/tenant/tables/%s/status", tableID),
			admin, map[string]any{"status": "OCCUPIED"}, tenantID)
		return status
	})
	wantOneWinner(t, "8 × occupy the table", got, 8)
}
