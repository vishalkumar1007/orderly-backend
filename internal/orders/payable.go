package orders

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/payable"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// PayableSource lets the payments framework pay for orders without importing
// this package.
func (h *Handler) PayableSource() payable.Source { return orderPayable{h} }

type orderPayable struct{ h *Handler }

func (orderPayable) ParseNumber(raw string) (int32, error) { return ParseOrderNumber(raw) }

func (o orderPayable) Resolve(ctx context.Context, tenantID uuid.UUID, number int32, customerID *uuid.UUID, phone string) (payable.Ref, error) {
	viewer := o.h.Viewer()
	var err error
	if customerID != nil {
		_, err = viewer.ByNumberForCustomer(ctx, tenantID, *customerID, number)
	} else {
		_, err = viewer.ByNumberAndPhone(ctx, tenantID, number, phone)
	}
	if err != nil {
		return payable.Ref{}, payable.ErrNotFound
	}
	row, err := o.raw(ctx, tenantID, number)
	if err != nil {
		return payable.Ref{}, err
	}
	return refOf(row), nil
}

func (o orderPayable) Load(ctx context.Context, tenantID uuid.UUID, id pgtype.UUID) (payable.Ref, error) {
	row, err := o.h.orderByPayment(ctx, tenantID, id)
	if err != nil {
		return payable.Ref{}, err
	}
	return refOf(row), nil
}

func refOf(row sqlc.Order) payable.Ref {
	return payable.Ref{
		Type:          payable.TypeOrder,
		ID:            row.ID,
		TenantID:      row.TenantID,
		Number:        row.OrderNumber,
		Amount:        row.Total,
		Active:        IsActive(row.Status),
		Status:        row.Status,
		StatusLabel:   StatusLabel(row.Status),
		CreatedAt:     row.CreatedAt.Time,
		CustomerEmail: row.CustomerEmail,
		CustomerPhone: row.CustomerPhone,
	}
}

func (o orderPayable) AfterPaid(ctx context.Context, tenantID uuid.UUID, ref payable.Ref) (string, string, error) {
	row, err := o.raw(ctx, tenantID, ref.Number)
	if err != nil {
		return "", "", err
	}
	view, err := o.h.AutoAcceptIfNeeded(ctx, tenantID, row)
	if err != nil {
		return "", "", err
	}
	return view.Status, StatusLabel(view.Status), nil
}

func (o orderPayable) Detail(ctx context.Context, tenantID uuid.UUID, ref payable.Ref) (map[string]any, error) {
	view, err := o.h.Viewer().ByNumber(ctx, tenantID, ref.Number)
	if err != nil {
		return nil, err
	}
	return view.Detail(), nil
}

func (o orderPayable) raw(ctx context.Context, tenantID uuid.UUID, number int32) (sqlc.Order, error) {
	return o.h.q.GetOrderByTenantAndNumber(ctx, sqlc.GetOrderByTenantAndNumberParams{
		TenantID:    pgutil.UUID(tenantID),
		OrderNumber: number,
	})
}
