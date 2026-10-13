package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Handler serves the console's inventory screen. Routes are mounted behind the
// menu permission, the catalog capability and the INVENTORY plan feature.
type Handler struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	log  *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, log *slog.Logger) *Handler {
	return &Handler{pool: pool, q: sqlc.New(pool), log: log}
}

const (
	maxQuantity = 1_000_000
	maxUnitLen  = 16
)

// Status values the console renders.
const (
	StatusOK        = "OK"
	StatusLow       = "LOW"
	StatusOut       = "OUT"
	StatusPaused    = "PAUSED"
	StatusUntracked = "UNTRACKED"
)

func statusOf(tracked, track bool, qty, threshold int32) string {
	switch {
	case !tracked:
		return StatusUntracked
	case !track:
		return StatusPaused
	case qty == 0:
		return StatusOut
	case threshold > 0 && qty <= threshold:
		return StatusLow
	}
	return StatusOK
}

func tenantOf(r *http.Request) (uuid.UUID, string, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return uuid.Nil, "", false
	}
	name := user.Name
	if name == "" {
		name = user.Email
	}
	return *user.TenantID, name, true
}

func productParam(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "productId"))
	return id, err == nil
}

// List returns every product with its stock state plus the counts the header shows.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	rows, err := h.q.ListInventoryOverview(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load inventory")
		return
	}
	counts := map[string]int{StatusOK: 0, StatusLow: 0, StatusOut: 0, StatusPaused: 0, StatusUntracked: 0}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		tracked := row.ItemID.Valid
		st := statusOf(tracked, row.Track.Bool, row.Quantity.Int32, row.ReorderThreshold.Int32)
		counts[st]++
		item := map[string]any{
			"product_id": pgutil.UUIDString(row.ProductID), "product_name": row.ProductName,
			"category": row.CategoryName, "price": pgutil.NumericToFloat(row.Price),
			"is_available": row.IsAvailable, "tracked": tracked, "status": st,
		}
		if tracked {
			item["quantity"] = row.Quantity.Int32
			item["unit"] = row.Unit.String
			item["reorder_threshold"] = row.ReorderThreshold.Int32
			item["track"] = row.Track.Bool
			item["auto_disabled"] = row.AutoDisabled.Bool
			item["updated_at"] = row.UpdatedAt.Time.Format(time.RFC3339)
		}
		items = append(items, item)
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": items, "summary": counts})
}

type trackRequest struct {
	// Quantity is the opening stock, used only when tracking starts.
	Quantity         *int32  `json:"quantity"`
	Unit             *string `json:"unit"`
	ReorderThreshold *int32  `json:"reorder_threshold"`
	Track            *bool   `json:"track"`
}

func cleanUnit(u string) (string, bool) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "pcs", true
	}
	return u, len([]rune(u)) <= maxUnitLen
}

// Track starts tracking a product, or changes its unit / reorder level / pause
// switch. Opening stock is only honoured when tracking starts; changing the
// quantity of a tracked product goes through Adjust so it lands in the ledger.
func (h *Handler) Track(w http.ResponseWriter, r *http.Request) {
	tenantID, actor, ok := tenantOf(r)
	productID, idOK := productParam(r)
	if !ok || !idOK {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid product id")
		return
	}
	var req trackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	for _, n := range []*int32{req.Quantity, req.ReorderThreshold} {
		if n != nil && (*n < 0 || *n > maxQuantity) {
			response.Error(w, http.StatusBadRequest, "invalid_request", "quantities must be between 0 and 1,000,000")
			return
		}
	}
	ctx := r.Context()
	tid := pgutil.UUID(tenantID)
	if _, err := h.q.GetProductByID(ctx, sqlc.GetProductByIDParams{ID: pgutil.UUID(productID), TenantID: tid}); err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update inventory")
		return
	}
	defer tx.Rollback(ctx)
	q := h.q.WithTx(tx)

	item, err := q.LockInventoryItemByProduct(ctx, sqlc.LockInventoryItemByProductParams{TenantID: tid, ProductID: pgutil.UUID(productID)})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Start tracking. A concurrent first Track loses to the unique index.
		unit, unitOK := cleanUnit(deref(req.Unit))
		if !unitOK {
			response.Error(w, http.StatusBadRequest, "invalid_request", "unit is too long")
			return
		}
		qty := int32(0)
		if req.Quantity != nil {
			qty = *req.Quantity
		}
		created, err := q.CreateInventoryItem(ctx, sqlc.CreateInventoryItemParams{
			TenantID: tid, ProductID: pgutil.UUID(productID), Quantity: qty, Unit: unit,
			ReorderThreshold: derefInt(req.ReorderThreshold),
		})
		if err != nil {
			response.Error(w, http.StatusConflict, "conflict", "this product is already being tracked")
			return
		}
		if qty > 0 {
			if _, err := q.InsertInventoryMovement(ctx, sqlc.InsertInventoryMovementParams{
				TenantID: tid, ItemID: created.ID, Delta: qty, QuantityAfter: qty,
				Reason: ReasonInitial, Actor: actor,
			}); err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "could not update inventory")
				return
			}
		}
		if qty == 0 { // tracked with nothing on the shelf: hidden until stocked
			if err := h.hideForZero(ctx, q, created); err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "could not update inventory")
				return
			}
		}
	case err != nil:
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update inventory")
		return
	default:
		if err := h.changeSettings(ctx, q, item, req); err != nil {
			var bad *badRequestError
			if errors.As(err, &bad) {
				response.Error(w, http.StatusBadRequest, "invalid_request", bad.msg)
				return
			}
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not update inventory")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update inventory")
		return
	}
	h.respondItem(w, r, tenantID, productID, http.StatusOK)
}

type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

// changeSettings applies unit / threshold / track edits to an existing item.
// Pausing tracking hands a stock-hidden product back to the owner; resuming
// tracking at zero hides it again.
func (h *Handler) changeSettings(ctx_ context.Context, q *sqlc.Queries, item sqlc.InventoryItem, req trackRequest) error {
	unit, threshold, track := item.Unit, item.ReorderThreshold, item.Track
	if req.Unit != nil {
		u, ok := cleanUnit(*req.Unit)
		if !ok {
			return &badRequestError{"unit is too long"}
		}
		unit = u
	}
	if req.ReorderThreshold != nil {
		threshold = *req.ReorderThreshold
	}
	if req.Track != nil {
		track = *req.Track
	}
	updated, err := q.UpdateInventorySettings(ctx_, sqlc.UpdateInventorySettingsParams{
		ID: item.ID, TenantID: item.TenantID, Unit: unit, ReorderThreshold: threshold, Track: track,
	})
	if err != nil {
		return err
	}
	switch {
	case item.Track && !track && item.AutoDisabled:
		if err := q.SetProductAvailable(ctx_, sqlc.SetProductAvailableParams{ID: item.ProductID, TenantID: item.TenantID, IsAvailable: true}); err != nil {
			return err
		}
		return q.SetInventoryAutoDisabled(ctx_, sqlc.SetInventoryAutoDisabledParams{ID: item.ID, TenantID: item.TenantID, AutoDisabled: false})
	case !item.Track && track && updated.Quantity == 0:
		return h.hideForZero(ctx_, q, updated)
	}
	return nil
}

// hideForZero hides a tracked product with no stock, remembering that stock
// (not the owner) did it.
func (h *Handler) hideForZero(ctx_ context.Context, q *sqlc.Queries, item sqlc.InventoryItem) error {
	hidden, err := q.HideProductForStock(ctx_, sqlc.HideProductForStockParams{ID: item.ProductID, TenantID: item.TenantID})
	if err != nil {
		return err
	}
	if hidden > 0 {
		return q.SetInventoryAutoDisabled(ctx_, sqlc.SetInventoryAutoDisabledParams{ID: item.ID, TenantID: item.TenantID, AutoDisabled: true})
	}
	return nil
}

type adjustRequest struct {
	Reason string `json:"reason"` // ADD | WASTE | CORRECTION
	// Quantity is how many to add or remove (ADD / WASTE), or the new total (CORRECTION).
	Quantity int32  `json:"quantity"`
	Note     string `json:"note"`
}

// Adjust changes a tracked product's stock and writes the ledger row.
func (h *Handler) Adjust(w http.ResponseWriter, r *http.Request) {
	tenantID, actor, ok := tenantOf(r)
	productID, idOK := productParam(r)
	if !ok || !idOK {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid product id")
		return
	}
	var req adjustRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	req.Reason = strings.ToUpper(strings.TrimSpace(req.Reason))
	req.Note = strings.TrimSpace(req.Note)
	if len([]rune(req.Note)) > 200 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "note is too long (200 characters at most)")
		return
	}
	switch req.Reason {
	case ReasonAdd, ReasonWaste:
		if req.Quantity <= 0 || req.Quantity > maxQuantity {
			response.Error(w, http.StatusBadRequest, "invalid_request", "enter a quantity between 1 and 1,000,000")
			return
		}
	case ReasonCorrection:
		if req.Quantity < 0 || req.Quantity > maxQuantity {
			response.Error(w, http.StatusBadRequest, "invalid_request", "enter a total between 0 and 1,000,000")
			return
		}
	default:
		response.Error(w, http.StatusBadRequest, "invalid_request", "reason must be ADD, WASTE or CORRECTION")
		return
	}

	ctx := r.Context()
	tid := pgutil.UUID(tenantID)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not adjust stock")
		return
	}
	defer tx.Rollback(ctx)
	q := h.q.WithTx(tx)

	item, err := q.LockInventoryItemByProduct(ctx, sqlc.LockInventoryItemByProductParams{TenantID: tid, ProductID: pgutil.UUID(productID)})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusNotFound, "not_tracked", "start tracking this product before adjusting its stock")
		return
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not adjust stock")
		return
	}
	product, err := q.GetProductByID(ctx, sqlc.GetProductByIDParams{ID: pgutil.UUID(productID), TenantID: tid})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}

	delta := req.Quantity
	switch req.Reason {
	case ReasonWaste:
		delta = -req.Quantity
	case ReasonCorrection:
		delta = req.Quantity - item.Quantity
	}
	if delta == 0 {
		response.Error(w, http.StatusBadRequest, "no_change", "the stock is already "+strconv.Itoa(int(item.Quantity)))
		return
	}
	if item.Quantity+delta > maxQuantity {
		response.Error(w, http.StatusBadRequest, "invalid_request", "that would take stock above 1,000,000")
		return
	}
	_, alert, _, err := apply(ctx, q, item, product.Name, delta, req.Reason, pgtype.UUID{}, req.Note, actor) // no order: a manual change
	var short *InsufficientError
	if errors.As(err, &short) {
		response.Error(w, http.StatusConflict, "insufficient_stock", short.Error())
		return
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not adjust stock")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not adjust stock")
		return
	}
	if alert != nil {
		Notify(ctx, notify.Deps{Q: h.q, Log: h.log}, tenantID, []Alert{*alert})
	}
	h.respondItem(w, r, tenantID, productID, http.StatusOK)
}

// Movements returns a product's ledger, newest first.
func (h *Handler) Movements(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	productID, idOK := productParam(r)
	if !ok || !idOK {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid product id")
		return
	}
	tid := pgutil.UUID(tenantID)
	item, err := h.q.GetInventoryItemByProduct(r.Context(), sqlc.GetInventoryItemByProductParams{TenantID: tid, ProductID: pgutil.UUID(productID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_tracked", "this product is not tracked")
		return
	}
	limit := int32(50)
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = int32(n)
	}
	rows, err := h.q.ListInventoryMovements(r.Context(), sqlc.ListInventoryMovementsParams{TenantID: tid, ItemID: item.ID, RowLimit: limit})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load history")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		row := map[string]any{
			"id": pgutil.UUIDString(m.ID), "delta": m.Delta, "quantity_after": m.QuantityAfter,
			"reason": m.Reason, "note": m.Note, "actor": m.Actor,
			"created_at": m.CreatedAt.Time.Format(time.RFC3339),
		}
		if m.OrderNumber.Valid {
			row["order_number"] = m.OrderNumber.Int32
		}
		out = append(out, row)
	}
	response.JSON(w, http.StatusOK, map[string]any{"movements": out})
}

// respondItem writes the overview row for one product.
func (h *Handler) respondItem(w http.ResponseWriter, r *http.Request, tenantID, productID uuid.UUID, status int) {
	rows, err := h.q.ListInventoryOverview(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load inventory")
		return
	}
	for _, row := range rows {
		if uuid.UUID(row.ProductID.Bytes) != productID {
			continue
		}
		tracked := row.ItemID.Valid
		out := map[string]any{
			"product_id": pgutil.UUIDString(row.ProductID), "product_name": row.ProductName,
			"is_available": row.IsAvailable, "tracked": tracked,
			"status": statusOf(tracked, row.Track.Bool, row.Quantity.Int32, row.ReorderThreshold.Int32),
		}
		if tracked {
			out["quantity"], out["unit"] = row.Quantity.Int32, row.Unit.String
			out["reorder_threshold"], out["track"] = row.ReorderThreshold.Int32, row.Track.Bool
			out["auto_disabled"] = row.AutoDisabled.Bool
		}
		response.JSON(w, status, out)
		return
	}
	response.Error(w, http.StatusNotFound, "not_found", "product not found")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int32) int32 {
	if n == nil {
		return 0
	}
	return *n
}
