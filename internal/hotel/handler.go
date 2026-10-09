package hotel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Handler struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	log  *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, log *slog.Logger) *Handler {
	return &Handler{pool: pool, q: sqlc.New(pool), log: log}
}

// customerContact looks up a guest's name and phone for the notification
// payload. Best-effort: a lookup problem never blocks the reservation.
func (h *Handler) customerContact(ctx context.Context, tenantID uuid.UUID, customerID pgtype.UUID) (name string, contact notify.Contact) {
	if !customerID.Valid {
		return "", notify.Contact{}
	}
	c, err := h.q.GetCustomerByID(ctx, sqlc.GetCustomerByIDParams{ID: customerID, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		return "", notify.Contact{}
	}
	phone := ""
	if c.Phone.Valid {
		phone = c.Phone.String
	}
	return c.Name, notify.Contact{Phone: phone}
}

// notifyReservation dispatches a hotel reservation event through the shared
// rules engine.
func (h *Handler) notifyReservation(ctx context.Context, tid uuid.UUID, res sqlc.Reservation, eventType, title string) {
	name, contact := h.customerContact(ctx, tid, res.CustomerID)
	notify.Dispatch(ctx, notify.Deps{Q: h.q, Log: h.log}, &tid, eventType, title, "",
		map[string]any{
			"reservation_id": pgutil.UUIDString(res.ID),
			"customer_name":  name,
			"check_in":       res.CheckInDate.Time,
			"check_out":      res.CheckOutDate.Time,
		},
		contact)
}

func tenantID(r *http.Request) uuid.UUID {
	u, _ := identity.UserFromContext(r.Context())
	return *u.TenantID
}

/* ------------------------------------------------------------------ *
 * Room types
 * ------------------------------------------------------------------ */

type roomTypeReq struct {
	Name      string   `json:"name"`
	BasePrice *float64 `json:"base_price"`
	MaxGuests *int32   `json:"max_guests"`
}

func (h *Handler) ListRoomTypes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListRoomTypesByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list room types")
		return
	}
	out := make([]any, 0, len(rows))
	for _, rt := range rows {
		out = append(out, roomTypeJSON(rt))
	}
	response.JSON(w, http.StatusOK, map[string]any{"room_types": out})
}

func (h *Handler) CreateRoomType(w http.ResponseWriter, r *http.Request) {
	var req roomTypeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" || req.BasePrice == nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name and base_price required")
		return
	}
	price, err := pgutil.NumericFromFloat(*req.BasePrice)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid base_price")
		return
	}
	maxGuests := int32(2)
	if req.MaxGuests != nil {
		maxGuests = *req.MaxGuests
	}
	rt, err := h.q.CreateRoomType(r.Context(), sqlc.CreateRoomTypeParams{
		TenantID: pgutil.UUID(tenantID(r)), Name: strings.TrimSpace(req.Name),
		BasePrice: price, MaxGuests: maxGuests,
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not create room type")
		return
	}
	response.JSON(w, http.StatusCreated, roomTypeJSON(rt))
}

func (h *Handler) UpdateRoomType(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req roomTypeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateRoomTypeParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r))}
	if strings.TrimSpace(req.Name) != "" {
		params.Name = pgtype.Text{String: strings.TrimSpace(req.Name), Valid: true}
	}
	if req.BasePrice != nil {
		price, err := pgutil.NumericFromFloat(*req.BasePrice)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid base_price")
			return
		}
		params.BasePrice = price
	}
	if req.MaxGuests != nil {
		params.MaxGuests = pgtype.Int4{Int32: *req.MaxGuests, Valid: true}
	}
	rt, err := h.q.UpdateRoomType(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "room type not found")
		return
	}
	response.JSON(w, http.StatusOK, roomTypeJSON(rt))
}

func (h *Handler) DeleteRoomType(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	if err := h.q.DeleteRoomType(r.Context(), sqlc.DeleteRoomTypeParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete room type")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func roomTypeJSON(rt sqlc.RoomType) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(rt.ID), "name": rt.Name,
		"base_price": pgutil.NumericToFloat(rt.BasePrice), "max_guests": rt.MaxGuests,
	}
}

/* ------------------------------------------------------------------ *
 * Rooms
 * ------------------------------------------------------------------ */

type roomReq struct {
	RoomTypeID string `json:"room_type_id"`
	Number     string `json:"number"`
}

func (h *Handler) ListRooms(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListRoomsByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list rooms")
		return
	}
	out := make([]any, 0, len(rows))
	for _, room := range rows {
		out = append(out, roomJSON(room))
	}
	response.JSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (h *Handler) CreateRoom(w http.ResponseWriter, r *http.Request) {
	var req roomReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomTypeID == "" || strings.TrimSpace(req.Number) == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "room_type_id and number required")
		return
	}
	roomTypeID, err := uuid.Parse(req.RoomTypeID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid room_type_id")
		return
	}
	room, err := h.q.CreateRoom(r.Context(), sqlc.CreateRoomParams{
		TenantID: pgutil.UUID(tenantID(r)), RoomTypeID: pgutil.UUID(roomTypeID), Number: strings.TrimSpace(req.Number),
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not create room")
		return
	}
	response.JSON(w, http.StatusCreated, roomJSON(room))
}

// SetRoomStatus applies a direct room-status move (e.g. housekeeping marking
// CLEANING → INSPECTION → AVAILABLE). Reservation-driven moves (RESERVED,
// OCCUPIED, CHECKOUT_PENDING) go through the reservation transitions below
// instead, so the two machines never disagree about who last moved a room.
func (h *Handler) SetRoomStatus(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	room, err := h.q.GetRoomByID(r.Context(), sqlc.GetRoomByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	if !CanTransitionRoom(room.Status, req.Status) {
		response.Error(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("cannot move a room from %s to %s", room.Status, req.Status))
		return
	}
	updated, err := h.q.UpdateRoomStatus(r.Context(), sqlc.UpdateRoomStatusParams{
		ID: room.ID, TenantID: pgutil.UUID(tid), Status: req.Status,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update room")
		return
	}
	response.JSON(w, http.StatusOK, roomJSON(updated))
}

func roomJSON(room sqlc.Room) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(room.ID), "room_type_id": pgutil.UUIDString(room.RoomTypeID),
		"number": room.Number, "status": room.Status,
	}
}

/* ------------------------------------------------------------------ *
 * Reservations
 * ------------------------------------------------------------------ */

type reservationReq struct {
	CustomerID   *string `json:"customer_id"`
	RoomTypeID   string  `json:"room_type_id"`
	CheckInDate  string  `json:"check_in_date"`  // "2006-01-02"
	CheckOutDate string  `json:"check_out_date"` // "2006-01-02"
	Guests       *int32  `json:"guests"`
}

func (h *Handler) ListReservations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListReservationsByTenant(r.Context(), sqlc.ListReservationsByTenantParams{
		TenantID: pgutil.UUID(tenantID(r)), Limit: 200,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list reservations")
		return
	}
	out := make([]any, 0, len(rows))
	for _, res := range rows {
		out = append(out, reservationJSON(res))
	}
	response.JSON(w, http.StatusOK, map[string]any{"reservations": out})
}

func (h *Handler) CreateReservation(w http.ResponseWriter, r *http.Request) {
	var req reservationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomTypeID == "" ||
		req.CheckInDate == "" || req.CheckOutDate == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "room_type_id, check_in_date and check_out_date required")
		return
	}
	roomTypeID, err := uuid.Parse(req.RoomTypeID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid room_type_id")
		return
	}
	checkIn, err := time.Parse("2006-01-02", req.CheckInDate)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid check_in_date")
		return
	}
	checkOut, err := time.Parse("2006-01-02", req.CheckOutDate)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid check_out_date")
		return
	}
	if !checkOut.After(checkIn) {
		response.Error(w, http.StatusBadRequest, "invalid_request", "check_out_date must be after check_in_date")
		return
	}
	var customerID uuid.UUID
	if req.CustomerID != nil {
		if customerID, err = uuid.Parse(*req.CustomerID); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid customer_id")
			return
		}
	}
	guests := int32(1)
	if req.Guests != nil {
		guests = *req.Guests
	}
	res, err := h.q.CreateReservation(r.Context(), sqlc.CreateReservationParams{
		TenantID: pgutil.UUID(tenantID(r)), CustomerID: pgutil.NullUUID(&customerID),
		RoomTypeID:   pgutil.UUID(roomTypeID),
		CheckInDate:  pgtype.Date{Time: checkIn, Valid: true},
		CheckOutDate: pgtype.Date{Time: checkOut, Valid: true},
		Guests:       guests,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not create reservation")
		return
	}
	h.notifyReservation(r.Context(), tenantID(r), res, notify.TypeReservationCreated, "Reservation booked")
	response.JSON(w, http.StatusCreated, reservationJSON(res))
}

func (h *Handler) GetReservation(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	res, err := h.q.GetReservationByID(r.Context(), sqlc.GetReservationByIDParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}
	response.JSON(w, http.StatusOK, reservationJSON(res))
}

// reservationAction maps a staff action to the state it moves a reservation
// to. checkin and checkout are separate endpoints below: they need extra
// input (which room) and side effects (moving the room, opening a folio).
var reservationAction = map[string]string{
	"confirm": ReservationConfirmed,
	"cancel":  ReservationCancelled,
	"noshow":  ReservationNoShow,
}

func (h *Handler) Transition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := reservationAction[action]
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
		res, err := h.q.GetReservationByID(r.Context(), sqlc.GetReservationByIDParams{
			ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid),
		})
		if err != nil {
			response.Error(w, http.StatusNotFound, "not_found", "reservation not found")
			return
		}
		if !CanTransitionReservation(res.Status, target) {
			response.Error(w, http.StatusConflict, "invalid_transition",
				fmt.Sprintf("cannot move a reservation from %s to %s", res.Status, target))
			return
		}
		updated, err := h.q.UpdateReservationStatus(r.Context(), sqlc.UpdateReservationStatusParams{
			ID: res.ID, TenantID: pgutil.UUID(tid), Status: target,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not update reservation")
			return
		}
		switch target {
		case ReservationConfirmed:
			h.notifyReservation(r.Context(), tid, updated, notify.TypeReservationConfirmed, "Reservation confirmed")
		case ReservationCancelled:
			h.notifyReservation(r.Context(), tid, updated, notify.TypeReservationCancelled, "Reservation cancelled")
		case ReservationNoShow:
			h.notifyReservation(r.Context(), tid, updated, notify.TypeReservationNoShow, "Reservation no-show")
		}
		response.JSON(w, http.StatusOK, reservationJSON(updated))
	}
}

type checkInReq struct {
	RoomID string `json:"room_id"`
}

// CheckIn assigns the physical room and moves both machines together: the
// reservation to CHECKED_IN, the room to OCCUPIED — and opens the folio the
// stay's charges and payments settle against.
func (h *Handler) CheckIn(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req checkInReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomID == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "room_id required")
		return
	}
	roomID, err := uuid.Parse(req.RoomID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid room_id")
		return
	}
	res, err := h.q.GetReservationByID(r.Context(), sqlc.GetReservationByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}
	if !CanTransitionReservation(res.Status, ReservationCheckedIn) {
		response.Error(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("cannot check in a reservation from %s", res.Status))
		return
	}
	room, err := h.q.GetRoomByID(r.Context(), sqlc.GetRoomByIDParams{ID: pgutil.UUID(roomID), TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "unknown room")
		return
	}
	if !CanTransitionRoom(room.Status, RoomOccupied) {
		response.Error(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("room is %s and cannot be occupied", room.Status))
		return
	}
	updated, err := h.q.CheckInReservation(r.Context(), sqlc.CheckInReservationParams{
		ID: res.ID, TenantID: pgutil.UUID(tid), RoomID: pgutil.UUID(roomID),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not check in")
		return
	}
	if _, err := h.q.UpdateRoomStatus(r.Context(), sqlc.UpdateRoomStatusParams{
		ID: room.ID, TenantID: pgutil.UUID(tid), Status: RoomOccupied,
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "checked in but could not update the room")
		return
	}
	folio, err := h.q.CreateFolio(r.Context(), sqlc.CreateFolioParams{TenantID: pgutil.UUID(tid), ReservationID: res.ID})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "checked in but could not open the folio")
		return
	}
	h.notifyReservation(r.Context(), tid, updated, notify.TypeReservationCheckedIn, "Guest checked in")
	response.JSON(w, http.StatusOK, map[string]any{
		"reservation": reservationJSON(updated), "folio": folioJSON(folio),
	})
}

// CheckOut closes the stay: the reservation moves to CHECKED_OUT, the room to
// CHECKOUT_PENDING (housekeeping takes it from there), and the folio is
// marked SETTLED. Collecting the final payment is a separate call against
// payments (payable_type='FOLIO') — checkout does not require it to have
// already happened, matching how a hotel actually settles a bill at the desk.
func (h *Handler) CheckOut(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	res, err := h.q.GetReservationByID(r.Context(), sqlc.GetReservationByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}
	if !CanTransitionReservation(res.Status, ReservationCheckedOut) {
		response.Error(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("cannot check out a reservation from %s", res.Status))
		return
	}
	updated, err := h.q.CheckOutReservation(r.Context(), sqlc.CheckOutReservationParams{ID: res.ID, TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not check out")
		return
	}
	if roomID := pgutil.UUIDPtr(res.RoomID); roomID != nil {
		rid, _ := uuid.Parse(*roomID)
		if room, err := h.q.GetRoomByID(r.Context(), sqlc.GetRoomByIDParams{ID: pgutil.UUID(rid), TenantID: pgutil.UUID(tid)}); err == nil &&
			CanTransitionRoom(room.Status, RoomCheckoutPending) {
			_, _ = h.q.UpdateRoomStatus(r.Context(), sqlc.UpdateRoomStatusParams{
				ID: room.ID, TenantID: pgutil.UUID(tid), Status: RoomCheckoutPending,
			})
		}
	}
	if folio, err := h.q.GetFolioByReservation(r.Context(), sqlc.GetFolioByReservationParams{
		ReservationID: res.ID, TenantID: pgutil.UUID(tid),
	}); err == nil {
		_, _ = h.q.SetFolioStatus(r.Context(), sqlc.SetFolioStatusParams{ID: folio.ID, TenantID: pgutil.UUID(tid), Status: "SETTLED"})
	}
	h.notifyReservation(r.Context(), tid, updated, notify.TypeReservationCheckedOut, "Guest checked out")
	response.JSON(w, http.StatusOK, reservationJSON(updated))
}

func reservationJSON(res sqlc.Reservation) map[string]any {
	out := map[string]any{
		"id": pgutil.UUIDString(res.ID), "room_type_id": pgutil.UUIDString(res.RoomTypeID),
		"check_in_date": res.CheckInDate.Time.Format("2006-01-02"), "check_out_date": res.CheckOutDate.Time.Format("2006-01-02"),
		"guests": res.Guests, "status": res.Status,
	}
	if id := pgutil.UUIDPtr(res.CustomerID); id != nil {
		out["customer_id"] = *id
	}
	if id := pgutil.UUIDPtr(res.RoomID); id != nil {
		out["room_id"] = *id
	}
	return out
}

/* ------------------------------------------------------------------ *
 * Folios and charges
 * ------------------------------------------------------------------ */

func (h *Handler) GetFolio(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
	reservationID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid reservation id")
		return
	}
	folio, err := h.q.GetFolioByReservation(r.Context(), sqlc.GetFolioByReservationParams{
		ReservationID: pgutil.UUID(reservationID), TenantID: pgutil.UUID(tid),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "no folio for this reservation")
		return
	}
	charges, err := h.q.ListChargesByFolio(r.Context(), sqlc.ListChargesByFolioParams{FolioID: folio.ID, TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list charges")
		return
	}
	total, err := h.q.FolioTotal(r.Context(), sqlc.FolioTotalParams{FolioID: folio.ID, TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to total the folio")
		return
	}
	out := make([]any, 0, len(charges))
	for _, c := range charges {
		out = append(out, chargeJSON(c))
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"folio": folioJSON(folio), "charges": out, "total": pgutil.NumericToFloat(total),
	})
}

type chargeReq struct {
	Description string   `json:"description"`
	Amount      *float64 `json:"amount"`
}

func (h *Handler) CreateCharge(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
	reservationID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid reservation id")
		return
	}
	var req chargeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Description) == "" || req.Amount == nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "description and amount required")
		return
	}
	folio, err := h.q.GetFolioByReservation(r.Context(), sqlc.GetFolioByReservationParams{
		ReservationID: pgutil.UUID(reservationID), TenantID: pgutil.UUID(tid),
	})
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "reservation has no open folio yet — check in first")
		return
	}
	amount, err := pgutil.NumericFromFloat(*req.Amount)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid amount")
		return
	}
	charge, err := h.q.CreateCharge(r.Context(), sqlc.CreateChargeParams{
		TenantID: pgutil.UUID(tid), FolioID: folio.ID, Description: strings.TrimSpace(req.Description), Amount: amount,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not add the charge")
		return
	}
	response.JSON(w, http.StatusCreated, chargeJSON(charge))
}

func folioJSON(f sqlc.Folio) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(f.ID), "reservation_id": pgutil.UUIDString(f.ReservationID), "status": f.Status,
	}
}

func chargeJSON(c sqlc.Charge) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(c.ID), "description": c.Description, "amount": pgutil.NumericToFloat(c.Amount),
	}
}

/* ------------------------------------------------------------------ *
 * Housekeeping
 * ------------------------------------------------------------------ */

type housekeepingReq struct {
	RoomID     string  `json:"room_id"`
	TaskType   string  `json:"task_type"`
	AssignedTo *string `json:"assigned_to"`
}

func (h *Handler) ListHousekeepingTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListOpenHousekeepingTasks(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list housekeeping tasks")
		return
	}
	out := make([]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, housekeepingTaskJSON(t))
	}
	response.JSON(w, http.StatusOK, map[string]any{"tasks": out})
}

func (h *Handler) CreateHousekeepingTask(w http.ResponseWriter, r *http.Request) {
	var req housekeepingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomID == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "room_id required")
		return
	}
	roomID, err := uuid.Parse(req.RoomID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid room_id")
		return
	}
	taskType := strings.ToUpper(strings.TrimSpace(req.TaskType))
	switch taskType {
	case "CLEANING", "INSPECTION", "MAINTENANCE":
	default:
		taskType = "CLEANING"
	}
	var assignedTo uuid.UUID
	if req.AssignedTo != nil {
		if assignedTo, err = uuid.Parse(*req.AssignedTo); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid assigned_to")
			return
		}
	}
	task, err := h.q.CreateHousekeepingTask(r.Context(), sqlc.CreateHousekeepingTaskParams{
		TenantID: pgutil.UUID(tenantID(r)), RoomID: pgutil.UUID(roomID), TaskType: taskType,
		AssignedTo: pgutil.NullUUID(&assignedTo),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not create the task")
		return
	}
	response.JSON(w, http.StatusCreated, housekeepingTaskJSON(task))
}

// CompleteHousekeepingTask marks a task DONE. It does not itself move the
// room's status — that stays an explicit SetRoomStatus call, so a finished
// cleaning task doesn't silently skip the inspection stage.
func (h *Handler) CompleteHousekeepingTask(w http.ResponseWriter, r *http.Request) {
	tid := tenantID(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	task, err := h.q.UpdateHousekeepingTaskStatus(r.Context(), sqlc.UpdateHousekeepingTaskStatusParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid), Status: "DONE",
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "task not found")
		return
	}
	response.JSON(w, http.StatusOK, housekeepingTaskJSON(task))
}

func housekeepingTaskJSON(t sqlc.HousekeepingTask) map[string]any {
	out := map[string]any{
		"id": pgutil.UUIDString(t.ID), "room_id": pgutil.UUIDString(t.RoomID),
		"task_type": t.TaskType, "status": t.Status,
	}
	if id := pgutil.UUIDPtr(t.AssignedTo); id != nil {
		out["assigned_to"] = *id
	}
	return out
}
