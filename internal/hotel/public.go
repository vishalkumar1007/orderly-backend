package hotel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/* ------------------------------------------------------------------ *
 * Storefront stays
 *
 * A guest searches dates, sees what each room type costs for the whole
 * stay and how many are left, and reserves signed in. Prices and
 * availability are computed here, never taken from the request.
 * ------------------------------------------------------------------ */

// stayRange is a validated check-in/check-out pair.
type stayRange struct {
	in, out time.Time
	nights  int
	guests  int32
}

// parseStay validates a date range against the business's today.
func parseStay(checkIn, checkOut, guests string, loc *time.Location) (stayRange, string) {
	in, err1 := time.ParseInLocation("2006-01-02", checkIn, loc)
	out, err2 := time.ParseInLocation("2006-01-02", checkOut, loc)
	if err1 != nil || err2 != nil {
		return stayRange{}, "dates must be YYYY-MM-DD"
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if in.Before(today) {
		return stayRange{}, "Check-in cannot be in the past."
	}
	if in.After(today.AddDate(0, 0, storefront.StayHorizonDays)) {
		return stayRange{}, "That date is too far ahead to book online."
	}
	nights := int(out.Sub(in).Hours()/24 + 0.5)
	if nights < 1 {
		return stayRange{}, "Check-out must be after check-in."
	}
	if nights > storefront.MaxNights {
		return stayRange{}, "Stays can be up to " + strconv.Itoa(storefront.MaxNights) + " nights."
	}
	g, err := strconv.Atoi(guests)
	if guests == "" {
		g, err = 1, nil
	}
	if err != nil || g < 1 || g > 50 {
		return stayRange{}, "Choose how many guests."
	}
	return stayRange{in: in, out: out, nights: nights, guests: int32(g)}, ""
}

func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

// stayStore resolves the business for a public request and refuses an
// unpublished storefront.
func (h *Handler) stayStore(w http.ResponseWriter, r *http.Request) (uuid.UUID, *time.Location, bool) {
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

// PublicRoomTypes lists room types; with dates it adds availability and the
// price of the whole stay.
func (h *Handler) PublicRoomTypes(w http.ResponseWriter, r *http.Request) {
	tid, loc, ok := h.stayStore(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("check_in") == "" && q.Get("check_out") == "" {
		rows, err := h.q.ListRoomTypesByTenant(r.Context(), pgutil.UUID(tid))
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not load rooms")
			return
		}
		out := make([]any, 0, len(rows))
		for _, rt := range rows {
			out = append(out, roomTypeJSON(rt))
		}
		response.JSON(w, http.StatusOK, map[string]any{"room_types": out})
		return
	}
	stay, msg := parseStay(q.Get("check_in"), q.Get("check_out"), q.Get("guests"), loc)
	if msg != "" {
		response.Error(w, http.StatusBadRequest, "invalid_dates", msg)
		return
	}
	rows, err := h.q.RoomTypeAvailability(r.Context(), sqlc.RoomTypeAvailabilityParams{
		TenantID: pgutil.UUID(tid), CheckIn: pgDate(stay.in), CheckOut: pgDate(stay.out),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not check availability")
		return
	}
	out := make([]any, 0, len(rows))
	for _, rt := range rows {
		price := pgutil.NumericToFloat(rt.BasePrice)
		out = append(out, map[string]any{
			"id": pgutil.UUIDString(rt.ID), "name": rt.Name, "description": rt.Description,
			"image_url": rt.ImageUrl, "base_price": price, "max_guests": rt.MaxGuests,
			"available": max(0, rt.TotalRooms-rt.Booked), "nights": stay.nights,
			"total": price * float64(stay.nights),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"room_types": out})
}

type reserveReq struct {
	RoomTypeID  string `json:"room_type_id"`
	CheckIn     string `json:"check_in"`
	CheckOut    string `json:"check_out"`
	Guests      int    `json:"guests"`
	Notes       string `json:"notes"`
	ClientToken string `json:"client_token"`
}

// PublicReserve reserves a room type for the signed-in guest. The room type
// row is locked for the transaction so two guests racing for the last room
// cannot both get it.
func (h *Handler) PublicReserve(w http.ResponseWriter, r *http.Request) {
	tid, loc, ok := h.stayStore(w, r)
	if !ok {
		return
	}
	customer, ok := auth.CustomerForTenant(r.Context(), tid)
	if !ok {
		response.Error(w, http.StatusUnauthorized, "sign_in_required", "sign in to reserve")
		return
	}
	var req reserveReq
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid reservation")
		return
	}
	req.Notes = strings.TrimSpace(req.Notes)
	if len(req.Notes) > 500 || len(req.ClientToken) > 80 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "notes are too long")
		return
	}
	stay, msg := parseStay(req.CheckIn, req.CheckOut, strconv.Itoa(req.Guests), loc)
	if msg != "" {
		response.Error(w, http.StatusBadRequest, "invalid_dates", msg)
		return
	}
	roomTypeID, err := uuid.Parse(req.RoomTypeID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "choose a room")
		return
	}
	if req.ClientToken != "" {
		if existing, err := h.q.GetReservationByClientToken(r.Context(), sqlc.GetReservationByClientTokenParams{
			TenantID: pgutil.UUID(tid), ClientToken: req.ClientToken,
		}); err == nil {
			h.respondCustomerReservation(w, r, tid, existing, http.StatusOK)
			return
		}
	}

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not reserve")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	qtx := h.q.WithTx(tx)
	if _, err := qtx.LockRoomType(r.Context(), sqlc.LockRoomTypeParams{ID: pgutil.UUID(roomTypeID), TenantID: pgutil.UUID(tid)}); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "choose a room")
		return
	}
	rows, err := qtx.RoomTypeAvailability(r.Context(), sqlc.RoomTypeAvailabilityParams{
		TenantID: pgutil.UUID(tid), CheckIn: pgDate(stay.in), CheckOut: pgDate(stay.out),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not reserve")
		return
	}
	var room *sqlc.RoomTypeAvailabilityRow
	for i := range rows {
		if rows[i].ID == pgutil.UUID(roomTypeID) {
			room = &rows[i]
		}
	}
	if room == nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "choose a room")
		return
	}
	if stay.guests > room.MaxGuests {
		response.Error(w, http.StatusBadRequest, "too_many_guests", "This room fits up to "+strconv.Itoa(int(room.MaxGuests))+" guests.")
		return
	}
	if room.TotalRooms-room.Booked < 1 {
		response.Error(w, http.StatusConflict, "sold_out", "That room was just taken for these dates. Pick another.")
		return
	}
	total, err := pgutil.NumericFromFloat(pgutil.NumericToFloat(room.BasePrice) * float64(stay.nights))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not reserve")
		return
	}
	res, err := qtx.CreateOnlineReservation(r.Context(), sqlc.CreateOnlineReservationParams{
		TenantID: pgutil.UUID(tid), CustomerID: pgutil.UUID(customer.ID), RoomTypeID: room.ID,
		CheckInDate: pgDate(stay.in), CheckOutDate: pgDate(stay.out), Guests: stay.guests,
		ClientToken: req.ClientToken, Notes: req.Notes, TotalAmount: total,
	})
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && req.ClientToken != "" {
			if existing, gerr := h.q.GetReservationByClientToken(r.Context(), sqlc.GetReservationByClientTokenParams{
				TenantID: pgutil.UUID(tid), ClientToken: req.ClientToken,
			}); gerr == nil {
				h.respondCustomerReservation(w, r, tid, existing, http.StatusOK)
				return
			}
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not reserve")
		return
	}
	h.notifyReservation(r.Context(), tid, res, notify.TypeReservationCreated, "Reservation booked online")
	h.respondCustomerReservation(w, r, tid, res, http.StatusCreated)
}

// canCancelStay: a guest can cancel online until the day before check-in.
func canCancelStay(status string, checkIn time.Time, loc *time.Location) bool {
	if !CanTransitionReservation(status, ReservationCancelled) {
		return false
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return today.Before(time.Date(checkIn.Year(), checkIn.Month(), checkIn.Day(), 0, 0, 0, 0, loc))
}

func customerReservationJSON(row sqlc.ListCustomerReservationsRow, loc *time.Location) map[string]any {
	out := map[string]any{
		"id": pgutil.UUIDString(row.ID), "room_type_id": pgutil.UUIDString(row.RoomTypeID),
		"room_type_name": row.RoomTypeName, "image_url": row.RoomTypeImage,
		"check_in": row.CheckInDate.Time.Format("2006-01-02"), "check_out": row.CheckOutDate.Time.Format("2006-01-02"),
		"guests": row.Guests, "status": row.Status, "notes": row.Notes, "total_amount": nil,
		"can_cancel": canCancelStay(row.Status, row.CheckInDate.Time, loc),
	}
	if row.TotalAmount.Valid {
		out["total_amount"] = pgutil.NumericToFloat(row.TotalAmount)
	}
	return out
}

func (h *Handler) respondCustomerReservation(w http.ResponseWriter, r *http.Request, tid uuid.UUID, res sqlc.Reservation, status int) {
	_, loc, ok := h.stayStoreQuiet(r, tid)
	if !ok {
		loc = time.UTC
	}
	rows, err := h.q.ListCustomerReservations(r.Context(), sqlc.ListCustomerReservationsParams{
		TenantID: pgutil.UUID(tid), CustomerID: res.CustomerID,
	})
	if err == nil {
		for _, row := range rows {
			if row.ID == res.ID {
				response.JSON(w, status, customerReservationJSON(row, loc))
				return
			}
		}
	}
	response.JSON(w, status, map[string]any{"id": pgutil.UUIDString(res.ID), "status": res.Status})
}

// stayStoreQuiet resolves the business timezone without writing a response.
func (h *Handler) stayStoreQuiet(r *http.Request, tid uuid.UUID) (uuid.UUID, *time.Location, bool) {
	sf, err := storefront.NewLoaderFromQueries(h.q).Load(r.Context(), tid)
	if err != nil || sf == nil {
		return tid, time.UTC, false
	}
	return tid, sf.OpeningHours.Location(), true
}

// guestHere returns the business on this host and the signed-in guest, only
// when the guest's token was issued by that business.
func guestHere(w http.ResponseWriter, r *http.Request) (uuid.UUID, auth.CustomerPrincipal, bool) {
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

// CustomerReservations lists the signed-in guest's stays.
func (h *Handler) CustomerReservations(w http.ResponseWriter, r *http.Request) {
	tid, guest, ok := guestHere(w, r)
	if !ok {
		return
	}
	_, loc, _ := h.stayStoreQuiet(r, tid)
	rows, err := h.q.ListCustomerReservations(r.Context(), sqlc.ListCustomerReservationsParams{
		TenantID: pgutil.UUID(tid), CustomerID: pgutil.UUID(guest.ID),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load reservations")
		return
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, customerReservationJSON(row, loc))
	}
	response.JSON(w, http.StatusOK, map[string]any{"reservations": out})
}

// CancelReservation lets a guest cancel their own stay before check-in day.
func (h *Handler) CancelReservation(w http.ResponseWriter, r *http.Request) {
	tid, guest, ok := guestHere(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}
	res, err := h.q.GetReservationByID(r.Context(), sqlc.GetReservationByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	if err != nil || pgutil.UUIDString(res.CustomerID) != guest.ID.String() {
		response.Error(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}
	_, loc, _ := h.stayStoreQuiet(r, tid)
	if !canCancelStay(res.Status, res.CheckInDate.Time, loc) {
		response.Error(w, http.StatusConflict, "too_late", "This stay can no longer be cancelled online. Call the hotel instead.")
		return
	}
	updated, err := h.q.UpdateReservationStatus(r.Context(), sqlc.UpdateReservationStatusParams{
		ID: res.ID, TenantID: res.TenantID, Status: ReservationCancelled, ExpectedStatus: res.Status,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusConflict, "invalid_transition", "This reservation just changed. Refresh and try again.")
		return
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not cancel")
		return
	}
	h.notifyReservation(r.Context(), tid, updated, notify.TypeReservationCancelled, "Reservation cancelled by the guest")
	h.respondCustomerReservation(w, r, tid, updated, http.StatusOK)
}
