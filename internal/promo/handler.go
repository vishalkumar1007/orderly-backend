package promo

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Handler serves the console's Offers & Loyalty screen. Mounted inside the
// marketing route group (storefront permission + MARKETING plan feature).
type Handler struct {
	q   *sqlc.Queries
	log *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, log *slog.Logger) *Handler {
	return &Handler{q: sqlc.New(pool), log: log}
}

func tenantOf(r *http.Request) (uuid.UUID, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return uuid.Nil, false
	}
	return *user.TenantID, true
}

func decode(r *http.Request, into any) bool {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(into) == nil
}

// PromotionInput is the form the console submits.
type PromotionInput struct {
	Code             string  `json:"code"`
	Name             string  `json:"name"`
	Kind             string  `json:"kind"`
	Value            float64 `json:"value"`
	MinSubtotal      float64 `json:"min_subtotal"`
	MaxDiscount      float64 `json:"max_discount"`
	StartsAt         string  `json:"starts_at"`
	EndsAt           string  `json:"ends_at"`
	MaxRedemptions   int32   `json:"max_redemptions"`
	PerCustomerLimit int32   `json:"per_customer_limit"`
	FirstOrderOnly   bool    `json:"first_order_only"`
	Active           *bool   `json:"active"`
}

func (in *PromotionInput) clean() (starts, ends pgtype.Timestamptz, err error) {
	in.Code = NormalizeCode(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	in.Kind = strings.ToUpper(strings.TrimSpace(in.Kind))
	switch {
	case in.Code == "" || len(in.Code) > 32:
		return starts, ends, errors.New("a code of 1 to 32 characters is required")
	case utf8.RuneCountInString(in.Name) > 80:
		return starts, ends, errors.New("the name must be 80 characters or fewer")
	case in.Kind != "PERCENT" && in.Kind != "FIXED":
		return starts, ends, errors.New("kind must be PERCENT or FIXED")
	case in.Value <= 0 || (in.Kind == "PERCENT" && in.Value > 100):
		return starts, ends, errors.New("enter a discount greater than zero (percent up to 100)")
	case in.MinSubtotal < 0 || in.MaxDiscount < 0 || in.MaxRedemptions < 0 || in.PerCustomerLimit < 0:
		return starts, ends, errors.New("limits cannot be negative")
	}
	for _, f := range in.Code {
		if !(f >= 'A' && f <= 'Z' || f >= '0' && f <= '9' || f == '-' || f == '_') {
			return starts, ends, errors.New("codes use letters, numbers, - and _ only")
		}
	}
	parse := func(s string) (pgtype.Timestamptz, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return pgtype.Timestamptz{}, nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return pgtype.Timestamptz{}, errors.New("dates must be RFC 3339")
		}
		return pgutil.Timestamptz(t), nil
	}
	if starts, err = parse(in.StartsAt); err != nil {
		return
	}
	if ends, err = parse(in.EndsAt); err != nil {
		return
	}
	if starts.Valid && ends.Valid && ends.Time.Before(starts.Time) {
		return starts, ends, errors.New("the offer must end after it starts")
	}
	return starts, ends, nil
}

func promotionView(p sqlc.Promotion, redemptions int64, discountTotal float64) map[string]any {
	ts := func(t pgtype.Timestamptz) any {
		if !t.Valid {
			return nil
		}
		return t.Time.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id": pgutil.UUIDString(p.ID), "code": p.Code, "name": p.Name, "kind": p.Kind,
		"value": pgutil.NumericToFloat(p.Value), "min_subtotal": pgutil.NumericToFloat(p.MinSubtotal),
		"max_discount": pgutil.NumericToFloat(p.MaxDiscount), "starts_at": ts(p.StartsAt), "ends_at": ts(p.EndsAt),
		"max_redemptions": p.MaxRedemptions, "per_customer_limit": p.PerCustomerLimit, "first_order_only": p.FirstOrderOnly,
		"active": p.Active, "redemptions": redemptions, "discount_total": discountTotal,
		"created_at": ts(p.CreatedAt), "updated_at": ts(p.UpdatedAt),
	}
}

func (h *Handler) ListPromotions(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	rows, err := h.q.ListPromotions(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load offers")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		p := sqlc.Promotion{
			ID: row.ID, TenantID: row.TenantID, Code: row.Code, Name: row.Name, Kind: row.Kind, Value: row.Value,
			MinSubtotal: row.MinSubtotal, MaxDiscount: row.MaxDiscount, StartsAt: row.StartsAt, EndsAt: row.EndsAt,
			MaxRedemptions: row.MaxRedemptions, PerCustomerLimit: row.PerCustomerLimit, FirstOrderOnly: row.FirstOrderOnly,
			Active: row.Active, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}
		out = append(out, promotionView(p, row.Redemptions, pgutil.NumericToFloat(row.DiscountTotal)))
	}
	response.JSON(w, http.StatusOK, map[string]any{"offers": out})
}

func (h *Handler) CreatePromotion(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var in PromotionInput
	if !decode(r, &in) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	starts, ends, err := in.clean()
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	value, _ := pgutil.NumericFromFloat(in.Value)
	minSub, _ := pgutil.NumericFromFloat(in.MinSubtotal)
	maxDisc, _ := pgutil.NumericFromFloat(in.MaxDiscount)
	active := in.Active == nil || *in.Active
	p, err := h.q.CreatePromotion(r.Context(), sqlc.CreatePromotionParams{
		TenantID: pgutil.UUID(tenantID), Code: in.Code, Name: in.Name, Kind: in.Kind, Value: value, MinSubtotal: minSub,
		MaxDiscount: maxDisc, StartsAt: starts, EndsAt: ends, MaxRedemptions: in.MaxRedemptions,
		PerCustomerLimit: in.PerCustomerLimit, FirstOrderOnly: in.FirstOrderOnly, Active: active,
	})
	if err != nil {
		if strings.Contains(err.Error(), "promotions_code_unique") {
			response.Error(w, http.StatusConflict, "code_taken", "You already have an offer with this code.")
			return
		}
		h.log.Error("promo: create", "error", err)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not create the offer")
		return
	}
	response.JSON(w, http.StatusCreated, promotionView(p, 0, 0))
}

func (h *Handler) UpdatePromotion(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	id, perr := uuid.Parse(chi.URLParam(r, "id"))
	if !ok || perr != nil {
		response.Error(w, http.StatusNotFound, "not_found", "offer not found")
		return
	}
	var in PromotionInput
	if !decode(r, &in) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	// The code is the offer's identity in printed material; it does not change.
	existing, err := h.q.GetPromotion(r.Context(), sqlc.GetPromotionParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "offer not found")
		return
	}
	in.Code = existing.Code
	starts, ends, err := in.clean()
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	value, _ := pgutil.NumericFromFloat(in.Value)
	minSub, _ := pgutil.NumericFromFloat(in.MinSubtotal)
	maxDisc, _ := pgutil.NumericFromFloat(in.MaxDiscount)
	active := existing.Active
	if in.Active != nil {
		active = *in.Active
	}
	p, err := h.q.UpdatePromotion(r.Context(), sqlc.UpdatePromotionParams{
		Name: in.Name, Kind: in.Kind, Value: value, MinSubtotal: minSub, MaxDiscount: maxDisc, StartsAt: starts, EndsAt: ends,
		MaxRedemptions: in.MaxRedemptions, PerCustomerLimit: in.PerCustomerLimit, FirstOrderOnly: in.FirstOrderOnly,
		Active: active, ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update the offer")
		return
	}
	used, _ := h.q.CountPromotionRedemptions(r.Context(), p.ID)
	response.JSON(w, http.StatusOK, promotionView(p, used, 0))
}

func (h *Handler) DeletePromotion(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	id, perr := uuid.Parse(chi.URLParam(r, "id"))
	if !ok || perr != nil {
		response.Error(w, http.StatusNotFound, "not_found", "offer not found")
		return
	}
	n, err := h.q.DeletePromotion(r.Context(), sqlc.DeletePromotionParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not delete the offer")
		return
	}
	if n == 0 {
		response.Error(w, http.StatusNotFound, "not_found", "offer not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ValidatePromotion lets the console try a code against a sample cart, the
// same rules the storefront applies.
func (h *Handler) ValidatePromotion(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req struct {
		Code     string  `json:"code"`
		Subtotal float64 `json:"subtotal"`
		Phone    string  `json:"phone"`
	}
	_ = decode(r, &req)
	applied, prob := Quote(r.Context(), h.q, req.Code, Context{TenantID: tenantID, Phone: strings.TrimSpace(req.Phone), Subtotal: req.Subtotal})
	if prob != nil {
		response.JSON(w, http.StatusOK, map[string]any{"valid": false, "code": prob.Code, "message": prob.Message})
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"valid": true, "discount": applied.Discount, "message": applied.Message})
}

/* ---------- loyalty ---------- */

func (h *Handler) GetLoyalty(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	prog := LoadProgram(r.Context(), h.q, tenantID)
	summary, err := h.q.LoyaltySummary(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load loyalty")
		return
	}
	members, _ := h.q.ListLoyaltyMembers(r.Context(), sqlc.ListLoyaltyMembersParams{TenantID: pgutil.UUID(tenantID), LimitCount: 50})
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]any{
			"customer_id": pgutil.UUIDString(m.CustomerID), "name": m.CustomerName, "phone": m.CustomerPhone.String,
			"balance": m.Balance, "lifetime_earned": m.LifetimeEarned, "updated_at": m.UpdatedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"program": prog, "summary": summary, "members": out,
		"outstanding_value": money(float64(summary.OutstandingPoints) * prog.PointValue),
	})
}

func (h *Handler) PutLoyalty(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var in Program
	if !decode(r, &in) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	switch {
	case in.PointsPer100 < 0 || in.PointsPer100 > 10000:
		response.Error(w, http.StatusBadRequest, "bad_request", "points per 100 must be between 0 and 10000")
		return
	case in.PointValue < 0 || in.PointValue > 1000:
		response.Error(w, http.StatusBadRequest, "bad_request", "point value must be between 0 and 1000")
		return
	case in.MinRedeemPoints < 0 || in.MaxRedeemPercent < 0 || in.MaxRedeemPercent > 100:
		response.Error(w, http.StatusBadRequest, "bad_request", "check the redemption limits")
		return
	}
	pv, _ := pgutil.NumericFromFloat(in.PointValue)
	row, err := h.q.UpsertLoyaltyProgram(r.Context(), sqlc.UpsertLoyaltyProgramParams{
		TenantID: pgutil.UUID(tenantID), Enabled: in.Enabled, PointsPer100: in.PointsPer100, PointValue: pv,
		MinRedeemPoints: in.MinRedeemPoints, MaxRedeemPercent: in.MaxRedeemPercent,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save the programme")
		return
	}
	response.JSON(w, http.StatusOK, Program{
		Enabled: row.Enabled, PointsPer100: row.PointsPer100, PointValue: pgutil.NumericToFloat(row.PointValue),
		MinRedeemPoints: row.MinRedeemPoints, MaxRedeemPercent: row.MaxRedeemPercent,
	})
}

// AdjustLoyalty lets an owner add or remove points by hand, with a note.
// The ledger row makes the adjustment auditable.
func (h *Handler) AdjustLoyalty(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req struct {
		CustomerID string `json:"customer_id"`
		Points     int32  `json:"points"`
		Note       string `json:"note"`
	}
	if !decode(r, &req) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	cid, err := uuid.Parse(req.CustomerID)
	if err != nil || req.Points == 0 || req.Points < -100000 || req.Points > 100000 {
		response.Error(w, http.StatusBadRequest, "bad_request", "customer_id and a non-zero points value are required")
		return
	}
	if _, err := h.q.GetCustomerByID(r.Context(), sqlc.GetCustomerByIDParams{ID: pgutil.UUID(cid), TenantID: pgutil.UUID(tenantID)}); err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	acct, err := h.q.LockLoyaltyAccount(r.Context(), sqlc.LockLoyaltyAccountParams{TenantID: pgutil.UUID(tenantID), CustomerID: pgutil.UUID(cid)})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load the account")
		return
	}
	if acct.Balance+req.Points < 0 {
		response.Error(w, http.StatusBadRequest, "bad_request", "the balance cannot go below zero")
		return
	}
	if _, err := h.q.InsertLoyaltyTransaction(r.Context(), sqlc.InsertLoyaltyTransactionParams{
		TenantID: pgutil.UUID(tenantID), AccountID: acct.ID, Kind: "ADJUST", Points: req.Points, Note: strings.TrimSpace(req.Note),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not record the adjustment")
		return
	}
	updated, err := h.q.ApplyLoyaltyDelta(r.Context(), sqlc.ApplyLoyaltyDeltaParams{Delta: req.Points, ID: acct.ID})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update the balance")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"balance": updated.Balance})
}

var _ = pgx.ErrNoRows
