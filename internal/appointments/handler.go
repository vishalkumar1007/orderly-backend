package appointments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Handler struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, q: sqlc.New(pool)}
}

// tenantID comes from the verified staff token, never from the request, so a
// tenant admin cannot address another tenant's services, appointments or
// queue — the same rule every other module in this codebase follows.
func tenantID(r *http.Request) uuid.UUID {
	u, _ := identity.UserFromContext(r.Context())
	return *u.TenantID
}

/* ------------------------------------------------------------------ *
 * Services
 * ------------------------------------------------------------------ */

type serviceReq struct {
	Name            string   `json:"name"`
	DurationMinutes *int32   `json:"duration_minutes"`
	Price           *float64 `json:"price"`
	IsActive        *bool    `json:"is_active"`
}

func (h *Handler) ListServices(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListServicesByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list services")
		return
	}
	out := make([]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, serviceJSON(s))
	}
	response.JSON(w, http.StatusOK, map[string]any{"services": out})
}

func (h *Handler) CreateService(w http.ResponseWriter, r *http.Request) {
	var req serviceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		strings.TrimSpace(req.Name) == "" || req.DurationMinutes == nil || *req.DurationMinutes <= 0 || req.Price == nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name, duration_minutes and price required")
		return
	}
	price, err := pgutil.NumericFromFloat(*req.Price)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid price")
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	svc, err := h.q.CreateService(r.Context(), sqlc.CreateServiceParams{
		TenantID: pgutil.UUID(tenantID(r)), Name: strings.TrimSpace(req.Name),
		DurationMinutes: *req.DurationMinutes, Price: price, IsActive: isActive,
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not create service")
		return
	}
	response.JSON(w, http.StatusCreated, serviceJSON(svc))
}

func (h *Handler) UpdateService(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req serviceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateServiceParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r))}
	if strings.TrimSpace(req.Name) != "" {
		params.Name = pgtype.Text{String: strings.TrimSpace(req.Name), Valid: true}
	}
	if req.DurationMinutes != nil {
		params.DurationMinutes = pgtype.Int4{Int32: *req.DurationMinutes, Valid: true}
	}
	if req.Price != nil {
		price, err := pgutil.NumericFromFloat(*req.Price)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid price")
			return
		}
		params.Price = price
	}
	if req.IsActive != nil {
		params.IsActive = pgtype.Bool{Bool: *req.IsActive, Valid: true}
	}
	svc, err := h.q.UpdateService(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "service not found")
		return
	}
	response.JSON(w, http.StatusOK, serviceJSON(svc))
}

func (h *Handler) DeleteService(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	if err := h.q.DeleteService(r.Context(), sqlc.DeleteServiceParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete service")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func serviceJSON(s sqlc.Service) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(s.ID), "name": s.Name,
		"duration_minutes": s.DurationMinutes, "price": pgutil.NumericToFloat(s.Price),
		"is_active": s.IsActive,
	}
}

/* ------------------------------------------------------------------ *
 * Staff availability
 * ------------------------------------------------------------------ */

type availabilityReq struct {
	StaffID   string `json:"staff_id"`
	Weekday   *int32 `json:"weekday"`
	StartTime string `json:"start_time"` // "HH:MM"
	EndTime   string `json:"end_time"`
}

func (h *Handler) ListStaffAvailability(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListStaffAvailabilityByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list availability")
		return
	}
	out := make([]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, availabilityJSON(a))
	}
	response.JSON(w, http.StatusOK, map[string]any{"availability": out})
}

func (h *Handler) CreateStaffAvailability(w http.ResponseWriter, r *http.Request) {
	var req availabilityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Weekday == nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "staff_id, weekday, start_time and end_time required")
		return
	}
	staffID, err := uuid.Parse(req.StaffID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid staff_id")
		return
	}
	start, err := parseClock(req.StartTime)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid start_time")
		return
	}
	end, err := parseClock(req.EndTime)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid end_time")
		return
	}
	row, err := h.q.CreateStaffAvailability(r.Context(), sqlc.CreateStaffAvailabilityParams{
		TenantID: pgutil.UUID(tenantID(r)), StaffID: pgutil.UUID(staffID),
		Weekday: *req.Weekday, StartTime: start, EndTime: end,
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not save availability")
		return
	}
	response.JSON(w, http.StatusCreated, availabilityJSON(row))
}

func (h *Handler) DeleteStaffAvailability(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	if err := h.q.DeleteStaffAvailability(r.Context(), sqlc.DeleteStaffAvailabilityParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete availability")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func availabilityJSON(a sqlc.StaffAvailability) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(a.ID), "staff_id": pgutil.UUIDString(a.StaffID),
		"weekday": a.Weekday, "start_time": formatClock(a.StartTime), "end_time": formatClock(a.EndTime),
	}
}

// parseClock/formatClock convert between "HH:MM" and pgtype.Time
// (microseconds since midnight). Local to this package: nothing else in the
// codebase stores a bare time-of-day.
func parseClock(s string) (pgtype.Time, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return pgtype.Time{}, err
	}
	micros := int64(t.Hour())*3600e6 + int64(t.Minute())*60e6
	return pgtype.Time{Microseconds: micros, Valid: true}, nil
}

func formatClock(t pgtype.Time) string {
	if !t.Valid {
		return ""
	}
	total := t.Microseconds / 1e6
	return fmt.Sprintf("%02d:%02d", total/3600, (total%3600)/60)
}

/* ------------------------------------------------------------------ *
 * Appointments
 * ------------------------------------------------------------------ */

type appointmentReq struct {
	CustomerID  *string `json:"customer_id"`
	ServiceID   string  `json:"service_id"`
	StaffID     *string `json:"staff_id"`
	ScheduledAt string  `json:"scheduled_at"` // RFC3339
}

func (h *Handler) ListAppointments(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListUpcomingAppointments(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list appointments")
		return
	}
	out := make([]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, appointmentJSON(a))
	}
	response.JSON(w, http.StatusOK, map[string]any{"appointments": out})
}

func (h *Handler) CreateAppointment(w http.ResponseWriter, r *http.Request) {
	var req appointmentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ServiceID == "" || req.ScheduledAt == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "service_id and scheduled_at required")
		return
	}
	serviceID, err := uuid.Parse(req.ServiceID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid service_id")
		return
	}
	scheduledAt, err := time.Parse(time.RFC3339, req.ScheduledAt)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid scheduled_at")
		return
	}
	var customerID, staffID uuid.UUID
	if req.CustomerID != nil {
		if customerID, err = uuid.Parse(*req.CustomerID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid customer_id")
			return
		}
	}
	if req.StaffID != nil {
		if staffID, err = uuid.Parse(*req.StaffID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid staff_id")
			return
		}
	}
	// Ownership check: the service must belong to this tenant, not just exist.
	if _, err := h.q.GetServiceByID(r.Context(), sqlc.GetServiceByIDParams{
		ID: pgutil.UUID(serviceID), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "unknown service")
		return
	}
	appt, err := h.q.CreateAppointment(r.Context(), sqlc.CreateAppointmentParams{
		TenantID: pgutil.UUID(tenantID(r)), CustomerID: pgutil.NullUUID(&customerID),
		ServiceID: pgutil.UUID(serviceID), StaffID: pgutil.NullUUID(&staffID),
		ScheduledAt: pgtype.Timestamptz{Time: scheduledAt, Valid: true},
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not create appointment")
		return
	}
	response.JSON(w, http.StatusCreated, appointmentJSON(appt))
}

func (h *Handler) GetAppointment(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	appt, err := h.q.GetAppointmentByID(r.Context(), sqlc.GetAppointmentByIDParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "appointment not found")
		return
	}
	response.JSON(w, http.StatusOK, appointmentJSON(appt))
}

// appointmentAction maps a staff action to the state it moves an appointment
// to. The target state comes from this fixed table, never from the request.
var appointmentAction = map[string]string{
	"confirm": StatusConfirmed,
	"checkin": StatusCheckedIn,
	"cancel":  StatusCancelled,
	"noshow":  StatusNoShow,
}

// Transition applies a staff action to an appointment, enforcing the
// workflow. "checkin" additionally joins the tenant's queue in the same
// request, matching the POC customer journey (Book → Check-in → Queue).
func (h *Handler) Transition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := appointmentAction[action]
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid_request", "unknown action")
			return
		}
		tid := tenantID(r)
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
			return
		}
		appt, err := h.q.GetAppointmentByID(r.Context(), sqlc.GetAppointmentByIDParams{
			ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid),
		})
		if err != nil {
			response.Error(w, http.StatusNotFound, "not_found", "appointment not found")
			return
		}
		if !CanTransition(appt.Status, target) {
			response.Error(w, http.StatusConflict, "invalid_transition",
				fmt.Sprintf("cannot move an appointment from %s to %s", appt.Status, target))
			return
		}
		updated, err := h.q.UpdateAppointmentStatus(r.Context(), sqlc.UpdateAppointmentStatusParams{
			ID: appt.ID, TenantID: pgutil.UUID(tid), Status: target,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not update appointment")
			return
		}
		if target == StatusCheckedIn {
			entry, err := h.joinQueue(r.Context(), tid, pgutil.NullUUID(&id), updated.CustomerID)
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "checked in but failed to join the queue")
				return
			}
			updated, err = h.q.UpdateAppointmentStatus(r.Context(), sqlc.UpdateAppointmentStatusParams{
				ID: appt.ID, TenantID: pgutil.UUID(tid), Status: StatusWaiting,
			})
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "could not update appointment")
				return
			}
			response.JSON(w, http.StatusOK, map[string]any{
				"appointment": appointmentJSON(updated), "queue_entry": queueEntryJSON(entry),
			})
			return
		}
		response.JSON(w, http.StatusOK, appointmentJSON(updated))
	}
}

func appointmentJSON(a sqlc.Appointment) map[string]any {
	out := map[string]any{
		"id": pgutil.UUIDString(a.ID), "service_id": pgutil.UUIDString(a.ServiceID),
		"scheduled_at": a.ScheduledAt.Time.Format(time.RFC3339), "status": a.Status,
	}
	if id := pgutil.UUIDPtr(a.CustomerID); id != nil {
		out["customer_id"] = *id
	}
	if id := pgutil.UUIDPtr(a.StaffID); id != nil {
		out["staff_id"] = *id
	}
	return out
}

/* ------------------------------------------------------------------ *
 * Queue — serves both walk-ins and checked-in appointment customers.
 * ------------------------------------------------------------------ */

type queueJoinReq struct {
	CustomerID *string `json:"customer_id"`
}

// JoinQueue is the walk-in entry point: no appointment, just a customer
// showing up. Transition("checkin") is the appointment entry point and calls
// joinQueue directly instead.
func (h *Handler) JoinQueue(w http.ResponseWriter, r *http.Request) {
	var req queueJoinReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	var customerID uuid.UUID
	if req.CustomerID != nil {
		var err error
		if customerID, err = uuid.Parse(*req.CustomerID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid customer_id")
			return
		}
	}
	entry, err := h.joinQueue(r.Context(), tenantID(r), pgtype.UUID{}, pgutil.NullUUID(&customerID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not join the queue")
		return
	}
	response.JSON(w, http.StatusCreated, queueEntryJSON(entry))
}

// joinQueue allocates the next queue number for today and inserts the entry.
//
// ponytail: MAX+1 with retry-on-conflict rather than a dedicated per-day
// counter table (like order_counters) — a front-desk walk-in queue is low
// concurrency. Upgrade to a counters table if two people start being handed
// the same number.
func (h *Handler) joinQueue(ctx context.Context, tenantID uuid.UUID, appointmentID, customerID pgtype.UUID) (sqlc.QueueEntry, error) {
	tid := pgutil.UUID(tenantID)
	const attempts = 5
	var lastErr error
	for i := 0; i < attempts; i++ {
		next, err := h.q.NextQueueNumberToday(ctx, tid)
		if err != nil {
			return sqlc.QueueEntry{}, err
		}
		entry, err := h.q.CreateQueueEntry(ctx, sqlc.CreateQueueEntryParams{
			TenantID: tid, AppointmentID: appointmentID, CustomerID: customerID, QueueNumber: next,
		})
		if err == nil {
			return entry, nil
		}
		if !isUniqueViolation(err) {
			return sqlc.QueueEntry{}, err
		}
		lastErr = err
	}
	return sqlc.QueueEntry{}, lastErr
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

/* ------------------------------------------------------------------ *
 * Queue listing and transitions
 * ------------------------------------------------------------------ */

func (h *Handler) ListQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListTodayQueue(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list the queue")
		return
	}
	out := make([]any, 0, len(rows))
	for _, q := range rows {
		out = append(out, queueEntryJSON(q))
	}
	response.JSON(w, http.StatusOK, map[string]any{"queue": out})
}

// queueAction maps a staff action to the state it moves a queue entry to.
var queueAction = map[string]string{
	"call":     QueueCalled,
	"start":    QueueInService,
	"complete": QueueCompleted,
	"cancel":   QueueCancelled,
}

// QueueTransition applies a staff action to a queue entry, enforcing the
// workflow, and keeps the linked appointment (if any) in step: IN_SERVICE and
// COMPLETED are states both machines share, so both move together.
func (h *Handler) QueueTransition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := queueAction[action]
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid_request", "unknown action")
			return
		}
		tid := tenantID(r)
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
			return
		}
		rows, err := h.q.ListTodayQueue(r.Context(), pgutil.UUID(tid))
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load the queue")
			return
		}
		var current sqlc.QueueEntry
		found := false
		for _, row := range rows {
			if uuid.UUID(row.ID.Bytes) == id {
				current, found = row, true
				break
			}
		}
		if !found {
			response.Error(w, http.StatusNotFound, "not_found", "queue entry not found")
			return
		}
		if !CanTransitionQueue(current.Status, target) {
			response.Error(w, http.StatusConflict, "invalid_transition",
				fmt.Sprintf("cannot move a queue entry from %s to %s", current.Status, target))
			return
		}
		updated, err := h.q.UpdateQueueEntryStatus(r.Context(), sqlc.UpdateQueueEntryStatusParams{
			ID: current.ID, TenantID: pgutil.UUID(tid), Status: target,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not update the queue")
			return
		}
		if apptID := pgutil.UUIDPtr(updated.AppointmentID); apptID != nil && (target == QueueInService || target == QueueCompleted) {
			aid, _ := uuid.Parse(*apptID)
			apptTarget := StatusInService
			if target == QueueCompleted {
				apptTarget = StatusCompleted
			}
			if appt, err := h.q.GetAppointmentByID(r.Context(), sqlc.GetAppointmentByIDParams{
				ID: pgutil.UUID(aid), TenantID: pgutil.UUID(tid),
			}); err == nil && CanTransition(appt.Status, apptTarget) {
				_, _ = h.q.UpdateAppointmentStatus(r.Context(), sqlc.UpdateAppointmentStatusParams{
					ID: appt.ID, TenantID: pgutil.UUID(tid), Status: apptTarget,
				})
			}
		}
		response.JSON(w, http.StatusOK, queueEntryJSON(updated))
	}
}

func queueEntryJSON(q sqlc.QueueEntry) map[string]any {
	out := map[string]any{
		"id": pgutil.UUIDString(q.ID), "queue_number": q.QueueNumber, "status": q.Status,
	}
	if id := pgutil.UUIDPtr(q.AppointmentID); id != nil {
		out["appointment_id"] = *id
	}
	if id := pgutil.UUIDPtr(q.CustomerID); id != nil {
		out["customer_id"] = *id
	}
	return out
}
