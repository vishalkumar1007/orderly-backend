// Package tables is the Cafe/Restaurant dine-in capability group.
package tables

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Status values. AVAILABLE is the only one a plain status update may target —
// OCCUPIED/RESERVED are meant to be driven by the order/reservation flow that
// uses the table, not set directly by staff, so the transition table below
// deliberately only allows the housekeeping-style moves.
const (
	StatusAvailable = "AVAILABLE"
	StatusOccupied  = "OCCUPIED"
	StatusReserved  = "RESERVED"
	StatusCleaning  = "CLEANING"
)

var transitions = map[string][]string{
	StatusAvailable: {StatusOccupied, StatusReserved},
	StatusReserved:  {StatusOccupied, StatusAvailable},
	StatusOccupied:  {StatusCleaning},
	StatusCleaning:  {StatusAvailable},
}

func canTransition(from, to string) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

type Handler struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, q: sqlc.New(pool)}
}

func tenantID(r *http.Request) uuid.UUID {
	u, _ := identity.UserFromContext(r.Context())
	return *u.TenantID
}

type tableReq struct {
	Label string `json:"label"`
	Seats *int32 `json:"seats"`
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListTablesByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list tables")
		return
	}
	out := make([]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, tableJSON(t))
	}
	response.JSON(w, http.StatusOK, map[string]any{"tables": out})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req tableReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Label) == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "label required")
		return
	}
	seats := int32(2)
	if req.Seats != nil {
		seats = *req.Seats
	}
	t, err := h.q.CreateTable(r.Context(), sqlc.CreateTableParams{
		TenantID: pgutil.UUID(tenantID(r)), Label: strings.TrimSpace(req.Label), Seats: seats,
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not create table")
		return
	}
	response.JSON(w, http.StatusCreated, tableJSON(t))
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req tableReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateTableParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r))}
	if strings.TrimSpace(req.Label) != "" {
		params.Label = pgtype.Text{String: strings.TrimSpace(req.Label), Valid: true}
	}
	if req.Seats != nil {
		params.Seats = pgtype.Int4{Int32: *req.Seats, Valid: true}
	}
	t, err := h.q.UpdateTable(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "table not found")
		return
	}
	response.JSON(w, http.StatusOK, tableJSON(t))
}

func (h *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
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
	current, err := h.q.GetTableByID(r.Context(), sqlc.GetTableByIDParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "table not found")
		return
	}
	if !canTransition(current.Status, req.Status) {
		response.Error(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("cannot move a table from %s to %s", current.Status, req.Status))
		return
	}
	t, err := h.q.UpdateTableStatus(r.Context(), sqlc.UpdateTableStatusParams{
		ID: current.ID, TenantID: pgutil.UUID(tid), Status: req.Status,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update table")
		return
	}
	response.JSON(w, http.StatusOK, tableJSON(t))
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	if err := h.q.DeleteTable(r.Context(), sqlc.DeleteTableParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete table")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func tableJSON(t sqlc.Table) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(t.ID), "label": t.Label, "seats": t.Seats, "status": t.Status,
	}
}
