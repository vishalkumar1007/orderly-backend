package appointments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/* ------------------------------------------------------------------ *
 * Storefront booking
 *
 * A customer picks a service, a day and a start time the slot engine
 * offered, and books it signed in. The business comes from the request
 * host, never the body; the customer token must belong to that business.
 * ------------------------------------------------------------------ */

// bookingStore resolves the business for a public request and refuses an
// unpublished storefront. It returns the business's timezone for slot maths.
func (h *Handler) bookingStore(w http.ResponseWriter, r *http.Request) (uuid.UUID, *time.Location, bool) {
	t, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return uuid.Nil, nil, false
	}
	sf, err := storefront.NewLoaderFromQueries(h.q).Load(r.Context(), t.ID)
	if err != nil || sf == nil || !sf.IsPublished {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return uuid.Nil, nil, false
	}
	return t.ID, sf.OpeningHours.Location(), true
}

// PublicServices lists what can be booked and who takes bookings.
func (h *Handler) PublicServices(w http.ResponseWriter, r *http.Request) {
	tid, _, ok := h.bookingStore(w, r)
	if !ok {
		return
	}
	services, err := h.q.ListActiveServices(r.Context(), pgutil.UUID(tid))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load services")
		return
	}
	staff, err := h.q.ListBookableStaff(r.Context(), pgutil.UUID(tid))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load staff")
		return
	}
	outServices := make([]map[string]any, 0, len(services))
	for _, s := range services {
		outServices = append(outServices, map[string]any{
			"id": pgutil.UUIDString(s.ID), "name": s.Name,
			"duration_minutes": s.DurationMinutes, "price": pgutil.NumericToFloat(s.Price),
		})
	}
	outStaff := make([]map[string]any, 0, len(staff))
	for _, s := range staff {
		outStaff = append(outStaff, map[string]any{"id": pgutil.UUIDString(s.ID), "name": s.Name})
	}
	response.JSON(w, http.StatusOK, map[string]any{"services": outServices, "staff": outStaff})
}

// slotQuery is everything the slot engine needs for one day.
type slotQuery struct {
	tenantID uuid.UUID
	service  sqlc.Service
	day      time.Time // midnight in the business's timezone
	staffID  string    // "" for anyone
	skipID   string    // an appointment to ignore (the one being moved)
}

// slotsFor runs the slot engine against the database. It is called both to
// offer times and, inside the booking transaction, to re-check one.
func slotsFor(ctx context.Context, q *sqlc.Queries, sq slotQuery, now time.Time) ([]slot, error) {
	rows, err := q.ListAvailabilityForWeekday(ctx, sqlc.ListAvailabilityForWeekdayParams{
		TenantID: pgutil.UUID(sq.tenantID), Weekday: int32(sq.day.Weekday()),
	})
	if err != nil {
		return nil, err
	}
	var windows []window
	for _, a := range rows {
		id := pgutil.UUIDString(a.StaffID)
		if sq.staffID != "" && id != sq.staffID {
			continue
		}
		windows = append(windows, window{
			staffID: id,
			start:   time.Duration(a.StartTime.Microseconds) * time.Microsecond,
			end:     time.Duration(a.EndTime.Microseconds) * time.Microsecond,
		})
	}
	if len(windows) == 0 {
		return nil, nil
	}
	next := sq.day.AddDate(0, 0, 1)
	taken, err := q.ListStaffBusy(ctx, sqlc.ListStaffBusyParams{
		TenantID: pgutil.UUID(sq.tenantID), Since: pgutil.Timestamptz(sq.day), Until: pgutil.Timestamptz(next),
	})
	if err != nil {
		return nil, err
	}
	busy := map[string][]span{}
	for _, b := range taken {
		if pgutil.UUIDString(b.ID) == sq.skipID {
			continue
		}
		id := pgutil.UUIDString(b.StaffID)
		busy[id] = append(busy[id], span{b.ScheduledAt.Time, b.EndsAt.Time})
	}
	dur := time.Duration(sq.service.DurationMinutes) * time.Minute
	earliest := now.Add(storefront.LeadTimeMinutes * time.Minute)
	latest := now.AddDate(0, 0, storefront.HorizonDays)
	return computeSlots(sq.day, windows, busy, dur, storefront.SlotStepMinutes*time.Minute, earliest, latest), nil
}

// activeService loads a bookable service of this business.
func (h *Handler) activeService(ctx context.Context, tid uuid.UUID, raw string) (sqlc.Service, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return sqlc.Service{}, false
	}
	s, err := h.q.GetServiceByID(ctx, sqlc.GetServiceByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	return s, err == nil && s.IsActive
}

// PublicSlots lists free start times for a service on one day.
func (h *Handler) PublicSlots(w http.ResponseWriter, r *http.Request) {
	tid, loc, ok := h.bookingStore(w, r)
	if !ok {
		return
	}
	service, ok := h.activeService(r.Context(), tid, r.URL.Query().Get("service_id"))
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_request", "choose a service")
		return
	}
	day, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), loc)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "date must be YYYY-MM-DD")
		return
	}
	staffID := r.URL.Query().Get("staff_id")
	if staffID != "" {
		if _, err := uuid.Parse(staffID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid staff_id")
			return
		}
	}
	slots, err := slotsFor(r.Context(), h.q, slotQuery{tenantID: tid, service: service, day: day, staffID: staffID}, time.Now())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load times")
		return
	}
	out := make([]map[string]any, 0, len(slots))
	for _, s := range slots {
		out = append(out, map[string]any{"start": s.Start.Format(time.RFC3339), "staff_id": s.StaffID})
	}
	response.JSON(w, http.StatusOK, map[string]any{"slots": out})
}

type bookReq struct {
	ServiceID   string `json:"service_id"`
	StaffID     string `json:"staff_id"`
	Start       string `json:"start"`
	Notes       string `json:"notes"`
	ClientToken string `json:"client_token"`
}

// errSlotTaken means the start time was free when offered and is not now.
var errSlotTaken = errors.New("slot taken")

// claim locks the chosen staff member's schedule for this transaction,
// re-runs the slot engine and returns the staff member who can take start.
// Two customers racing for one time serialise on the lock, and the second
// sees the first's booking as busy.
func claim(ctx context.Context, q *sqlc.Queries, sq slotQuery, start time.Time) (string, error) {
	// Without a chosen staff member, pick whoever the engine offers first,
	// then lock that person and check again.
	if sq.staffID == "" {
		slots, err := slotsFor(ctx, q, sq, time.Now())
		if err != nil {
			return "", err
		}
		for _, s := range slots {
			if s.Start.Equal(start) {
				sq.staffID = s.StaffID
				break
			}
		}
		if sq.staffID == "" {
			return "", errSlotTaken
		}
	}
	if err := q.LockStaffSchedule(ctx, sq.tenantID.String()+":"+sq.staffID); err != nil {
		return "", err
	}
	slots, err := slotsFor(ctx, q, sq, time.Now())
	if err != nil {
		return "", err
	}
	for _, s := range slots {
		if s.Start.Equal(start) && s.StaffID == sq.staffID {
			return sq.staffID, nil
		}
	}
	return "", errSlotTaken
}

func decodeStrict(r *http.Request, target any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

// PublicBook books a service for the signed-in customer.
func (h *Handler) PublicBook(w http.ResponseWriter, r *http.Request) {
	tid, loc, ok := h.bookingStore(w, r)
	if !ok {
		return
	}
	customer, ok := auth.CustomerForTenant(r.Context(), tid)
	if !ok {
		response.Error(w, http.StatusUnauthorized, "sign_in_required", "sign in to book")
		return
	}
	var req bookReq
	if err := decodeStrict(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid booking")
		return
	}
	req.Notes = strings.TrimSpace(req.Notes)
	if len(req.Notes) > 500 || len(req.ClientToken) > 80 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "notes are too long")
		return
	}
	service, ok := h.activeService(r.Context(), tid, req.ServiceID)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_request", "choose a service")
		return
	}
	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid start time")
		return
	}
	if req.StaffID != "" {
		if _, err := uuid.Parse(req.StaffID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid staff_id")
			return
		}
	}
	// A repeated tap or a retry after a lost response returns the original.
	if req.ClientToken != "" {
		if existing, err := h.q.GetAppointmentByClientToken(r.Context(), sqlc.GetAppointmentByClientTokenParams{
			TenantID: pgutil.UUID(tid), ClientToken: req.ClientToken,
		}); err == nil {
			h.respondCustomerAppointment(w, r, tid, existing, http.StatusOK)
			return
		}
	}

	local := start.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not book")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	qtx := h.q.WithTx(tx)
	staffID, err := claim(r.Context(), qtx, slotQuery{tenantID: tid, service: service, day: day, staffID: req.StaffID}, start)
	if errors.Is(err, errSlotTaken) {
		response.Error(w, http.StatusConflict, "slot_taken", "That time was just taken. Pick another.")
		return
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not book")
		return
	}
	staffUUID, _ := uuid.Parse(staffID)
	appt, err := qtx.CreateOnlineAppointment(r.Context(), sqlc.CreateOnlineAppointmentParams{
		TenantID: pgutil.UUID(tid), CustomerID: pgutil.UUID(customer.ID), StaffID: pgutil.UUID(staffUUID),
		ScheduledAt: pgutil.Timestamptz(start), ClientToken: req.ClientToken, Notes: req.Notes,
		ServiceID: service.ID,
	})
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if isUniqueViolation(err) && req.ClientToken != "" {
			if existing, gerr := h.q.GetAppointmentByClientToken(r.Context(), sqlc.GetAppointmentByClientTokenParams{
				TenantID: pgutil.UUID(tid), ClientToken: req.ClientToken,
			}); gerr == nil {
				h.respondCustomerAppointment(w, r, tid, existing, http.StatusOK)
				return
			}
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not book")
		return
	}
	h.notifyAppointment(r.Context(), tid, appt, notify.TypeAppointmentCreated, "Appointment booked online")
	h.respondCustomerAppointment(w, r, tid, appt, http.StatusCreated)
}

// customerAppointmentJSON is what a customer sees of their own booking.
func customerAppointmentJSON(a sqlc.ListCustomerAppointmentsRow, now time.Time) map[string]any {
	active := a.Status == StatusRequested || a.Status == StatusConfirmed
	beforeCutoff := a.ScheduledAt.Time.Sub(now) >= storefront.CancelWindowMinutes*time.Minute
	return map[string]any{
		"id": pgutil.UUIDString(a.ID), "service_id": pgutil.UUIDString(a.ServiceID),
		"service_name": a.ServiceName, "price": pgutil.NumericToFloat(a.ServicePrice),
		"duration_minutes": a.DurationMinutes,
		"staff_id":         pgutil.UUIDString(a.StaffID), "staff_name": a.StaffName,
		"scheduled_at": a.ScheduledAt.Time.Format(time.RFC3339), "ends_at": a.EndsAt.Time.Format(time.RFC3339),
		"status": a.Status, "notes": a.Notes,
		"can_cancel": active && beforeCutoff, "can_reschedule": active && beforeCutoff,
	}
}

// respondCustomerAppointment answers with the enriched row for one booking.
func (h *Handler) respondCustomerAppointment(w http.ResponseWriter, r *http.Request, tid uuid.UUID, a sqlc.Appointment, status int) {
	row, err := h.q.GetCustomerAppointment(r.Context(), sqlc.GetCustomerAppointmentParams{
		TenantID: pgutil.UUID(tid), ID: a.ID,
	})
	if err != nil {
		response.JSON(w, status, appointmentJSON(a))
		return
	}
	response.JSON(w, status, customerAppointmentJSON(sqlc.ListCustomerAppointmentsRow(row), time.Now()))
}

// CustomerAppointments lists the signed-in customer's bookings, newest first.
func (h *Handler) CustomerAppointments(w http.ResponseWriter, r *http.Request) {
	tid, customer, ok := customerHere(w, r)
	if !ok {
		return
	}
	rows, err := h.q.ListCustomerAppointments(r.Context(), sqlc.ListCustomerAppointmentsParams{
		TenantID: pgutil.UUID(tid), CustomerID: pgutil.UUID(customer.ID),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load bookings")
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, customerAppointmentJSON(a, now))
	}
	response.JSON(w, http.StatusOK, map[string]any{"appointments": out})
}

// customerHere returns the business on this host and the signed-in customer,
// only when the customer's token was issued by that business.
func customerHere(w http.ResponseWriter, r *http.Request) (uuid.UUID, auth.CustomerPrincipal, bool) {
	t, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return uuid.Nil, auth.CustomerPrincipal{}, false
	}
	c, ok := auth.CustomerForTenant(r.Context(), t.ID)
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in again")
		return uuid.Nil, auth.CustomerPrincipal{}, false
	}
	return t.ID, c, true
}

// ownAppointment loads a booking that belongs to the signed-in customer.
// Anyone else's booking reads as not found.
func (h *Handler) ownAppointment(w http.ResponseWriter, r *http.Request) (uuid.UUID, sqlc.Appointment, bool) {
	tid, customer, ok := customerHere(w, r)
	if !ok {
		return uuid.Nil, sqlc.Appointment{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "booking not found")
		return uuid.Nil, sqlc.Appointment{}, false
	}
	appt, err := h.q.GetAppointmentByID(r.Context(), sqlc.GetAppointmentByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	if err != nil || pgutil.UUIDString(appt.CustomerID) != customer.ID.String() {
		response.Error(w, http.StatusNotFound, "not_found", "booking not found")
		return uuid.Nil, sqlc.Appointment{}, false
	}
	return tid, appt, true
}

func tooLate(a sqlc.Appointment) bool {
	return time.Until(a.ScheduledAt.Time) < storefront.CancelWindowMinutes*time.Minute
}

// CancelAppointment lets a customer cancel their own booking before the cutoff.
func (h *Handler) CancelAppointment(w http.ResponseWriter, r *http.Request) {
	tid, appt, ok := h.ownAppointment(w, r)
	if !ok {
		return
	}
	if !CanTransition(appt.Status, StatusCancelled) {
		response.Error(w, http.StatusConflict, "invalid_transition", "This booking can no longer be cancelled.")
		return
	}
	if tooLate(appt) {
		response.Error(w, http.StatusConflict, "too_late", "It is too close to the time to cancel online. Call the business instead.")
		return
	}
	updated, err := h.q.UpdateAppointmentStatus(r.Context(), sqlc.UpdateAppointmentStatusParams{
		ID: appt.ID, TenantID: appt.TenantID, Status: StatusCancelled, ExpectedStatus: appt.Status,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusConflict, "invalid_transition", "This booking just changed. Refresh and try again.")
		return
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not cancel")
		return
	}
	h.notifyAppointment(r.Context(), tid, updated, notify.TypeAppointmentCancelled, "Appointment cancelled by the customer")
	h.respondCustomerAppointment(w, r, tid, updated, http.StatusOK)
}

type rescheduleReq struct {
	Start   string `json:"start"`
	StaffID string `json:"staff_id"`
}

// RescheduleAppointment moves a customer's booking to another free time.
func (h *Handler) RescheduleAppointment(w http.ResponseWriter, r *http.Request) {
	tid, appt, ok := h.ownAppointment(w, r)
	if !ok {
		return
	}
	if appt.Status != StatusRequested && appt.Status != StatusConfirmed {
		response.Error(w, http.StatusConflict, "invalid_transition", "This booking can no longer be moved.")
		return
	}
	if tooLate(appt) {
		response.Error(w, http.StatusConflict, "too_late", "It is too close to the time to change online. Call the business instead.")
		return
	}
	var req rescheduleReq
	if err := decodeStrict(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid time")
		return
	}
	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid start time")
		return
	}
	if req.StaffID != "" {
		if _, err := uuid.Parse(req.StaffID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid staff_id")
			return
		}
	}
	service, err := h.q.GetServiceByID(r.Context(), sqlc.GetServiceByIDParams{ID: appt.ServiceID, TenantID: appt.TenantID})
	if err != nil {
		response.Error(w, http.StatusConflict, "invalid_request", "This service is no longer offered.")
		return
	}
	_, loc, ok := h.bookingStore(w, r)
	if !ok {
		return
	}
	local := start.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not change the booking")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	qtx := h.q.WithTx(tx)
	sq := slotQuery{tenantID: tid, service: service, day: day, staffID: req.StaffID, skipID: pgutil.UUIDString(appt.ID)}
	staffID, err := claim(r.Context(), qtx, sq, start)
	if errors.Is(err, errSlotTaken) {
		response.Error(w, http.StatusConflict, "slot_taken", "That time was just taken. Pick another.")
		return
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not change the booking")
		return
	}
	staffUUID, _ := uuid.Parse(staffID)
	moved, err := qtx.RescheduleAppointment(r.Context(), sqlc.RescheduleAppointmentParams{
		ID: appt.ID, TenantID: appt.TenantID, ScheduledAt: pgutil.Timestamptz(start), StaffID: pgutil.UUID(staffUUID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusConflict, "invalid_transition", "This booking just changed. Refresh and try again.")
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not change the booking")
		return
	}
	h.notifyAppointment(r.Context(), tid, moved, notify.TypeAppointmentCreated, "Appointment moved by the customer")
	h.respondCustomerAppointment(w, r, tid, moved, http.StatusOK)
}
