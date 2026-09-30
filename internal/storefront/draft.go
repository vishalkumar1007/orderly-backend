package storefront

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/*
Storefront drafts.

The Studio lets an owner try several things and publish once. Two properties
make that safe, and neither was true when the draft lived in the browser:

  - a draft belongs to the shop. It is readable from any device the owner signs
    in on, it survives a crash, and a colleague can be told that somebody has
    work in progress.
  - publishing is one transaction. Applying a draft touches identity, theme,
    homepage, payments, workflow, costing and publish state; doing that as six
    HTTP calls meant a failure halfway left a shop configured half old and half
    new, with nothing to roll back to.

The draft document is the same shape the admin API already returns, so the
screen edits, stores and publishes exactly what it renders. Every value still
goes through the same validation and the same document parsers on the way in —
a draft is untrusted input like any other, and one that has been sitting in a
table for a week is no more trustworthy than one that just arrived.
*/

// draftDocument is the subset of the admin payload a draft can change.
//
// Pointers throughout: a draft written by an older client simply does not carry
// the fields it never knew about, and publishing must leave those alone rather
// than blanking them.
type draftDocument struct {
	Store *struct {
		Name        *string `json:"name"`
		LogoURL     *string `json:"logo_url"`
		FaviconURL  *string `json:"favicon_url"`
		Tagline     *string `json:"tagline"`
		Description *string `json:"description"`
		Phone       *string `json:"phone"`
		Address     *string `json:"address"`
	} `json:"store"`

	Theme *struct {
		Preset             *string `json:"preset"`
		Mode               *string `json:"mode"`
		Font               *string `json:"font"`
		Radius             *string `json:"radius"`
		Button             *string `json:"button"`
		Card               *string `json:"card"`
		Header             *string `json:"header"`
		Hero               *string `json:"hero"`
		ProductLayout      *string `json:"product_layout"`
		FilterStyle        *string `json:"filter_style"`
		Primary            *string `json:"primary"`
		Secondary          *string `json:"secondary"`
		Accent             *string `json:"accent"`
		HeroImageURL       *string `json:"hero_image_url"`
		CustomerModeSwitch *bool   `json:"customer_mode_switch_enabled"`
	} `json:"theme"`

	Hours *struct {
		AlwaysOpen *bool          `json:"always_open"`
		Timezone   *string        `json:"timezone"`
		Schedule   map[string]any `json:"schedule"`
	} `json:"hours"`

	Behaviour *struct {
		OrderingEnabled   *bool   `json:"ordering_enabled"`
		ClosedMessage     *string `json:"closed_message"`
		CustomerLoginMode *string `json:"customer_login_mode"`
		PrepTimeMinutes   *int    `json:"prep_time_minutes"`
		TaxPercent        any     `json:"tax_percent"`
		PackagingFee      any     `json:"packaging_fee"`
		Published         *bool   `json:"published"`
		StoreStatus       *string `json:"store_status"`
		StatusMessage     *string `json:"status_message"`
	} `json:"behaviour"`

	Homepage *struct {
		Sections json.RawMessage `json:"sections"`
	} `json:"homepage"`

	Payments json.RawMessage `json:"payments"`
	Workflow json.RawMessage `json:"workflow"`
}

// GetDraft returns the shop's draft, or null when there is none.
func (a *AdminHandler) GetDraft(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}

	row, err := a.q.GetStorefrontDraft(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.JSON(w, http.StatusOK, map[string]any{
				"draft":        nil,
				"live_version": sf.UpdatedAt,
			})
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your draft")
		return
	}

	var doc json.RawMessage = row.Document
	response.JSON(w, http.StatusOK, map[string]any{
		"draft":        doc,
		"base_version": row.BaseVersion.Time,
		"updated_at":   row.UpdatedAt.Time,
		"updated_by":   a.draftAuthor(r.Context(), row.UpdatedBy),
		"live_version": sf.UpdatedAt,
		// True when the shop moved on after this draft was started. The Studio
		// shows it rather than discovering it at publish time.
		"stale": sf.UpdatedAt.After(row.BaseVersion.Time),
	})
}

// PutDraft saves the Studio's working copy. This is the autosave.
func (a *AdminHandler) PutDraft(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}

	var body json.RawMessage
	if err := decodeAdmin(r, &body); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read that draft")
		return
	}
	// Parse it now, even though nothing is applied yet: a draft that cannot be
	// published is not worth storing, and finding out at publish time turns a
	// typo into lost work.
	var doc draftDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "That draft is not a storefront")
		return
	}

	// The version the draft is measured against is fixed when it is created, so
	// a running autosave cannot quietly move the goalposts for the conflict
	// check at publish time.
	base := pgtype.Timestamptz{Time: sf.UpdatedAt, Valid: true}
	if existing, err := a.q.GetStorefrontDraft(r.Context(), pgutil.UUID(tenantID)); err == nil {
		base = existing.BaseVersion
	}

	actor, _ := identity.UserFromContext(r.Context())
	row, err := a.q.UpsertStorefrontDraft(r.Context(), sqlc.UpsertStorefrontDraftParams{
		TenantID:    pgutil.UUID(tenantID),
		Document:    body,
		BaseVersion: base,
		UpdatedBy:   pgutil.UUID(actor.ID),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your draft")
		return
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"updated_at":   row.UpdatedAt.Time,
		"base_version": row.BaseVersion.Time,
		"live_version": sf.UpdatedAt,
		"stale":        sf.UpdatedAt.After(row.BaseVersion.Time),
	})
}

// DeleteDraft discards the working copy and leaves the live shop alone.
func (a *AdminHandler) DeleteDraft(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	if err := a.q.DeleteStorefrontDraft(r.Context(), pgutil.UUID(tenantID)); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not discard your draft")
		return
	}
	a.respond(w, r, tenantID)
}

// PublishDraft applies the draft to the live storefront, all of it or none.
func (a *AdminHandler) PublishDraft(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}

	row, err := a.q.GetStorefrontDraft(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAdminError(w, "no_draft", "There is nothing to publish")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your draft")
		return
	}

	// `force` is the answer to "publish anyway" after the console has shown the
	// owner that the shop moved on. Without it a stale draft is refused.
	force := strings.EqualFold(r.URL.Query().Get("force"), "true")
	if !force && sf.UpdatedAt.After(row.BaseVersion.Time) {
		response.Error(w, http.StatusConflict, "draft_conflict",
			"Your storefront changed since this draft was started. Review the changes before publishing.")
		return
	}

	var doc draftDocument
	if err := json.Unmarshal(row.Document, &doc); err != nil {
		writeAdminError(w, "invalid_draft", "That draft could not be read. Discard it and start again.")
		return
	}

	tx, err := a.pool.Begin(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not start publishing")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	q := a.q.WithTx(tx)
	if err := a.applyDraft(r.Context(), q, tenantID, sf, doc); err != nil {
		if msg, code, ok := draftFieldError(err); ok {
			writeAdminError(w, code, msg)
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not publish your changes")
		return
	}
	if err := q.DeleteStorefrontDraft(r.Context(), pgutil.UUID(tenantID)); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not publish your changes")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not publish your changes")
		return
	}

	a.respond(w, r, tenantID)
}

func (a *AdminHandler) draftAuthor(ctx context.Context, id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	user, err := a.q.GetUserByID(ctx, id)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(user.Name) != "" {
		return user.Name
	}
	return user.Email
}

// draftFieldError turns a validation failure into something the screen can put
// next to the field that caused it.
func draftFieldError(err error) (msg string, code string, ok bool) {
	var fe *fieldError
	if errors.As(err, &fe) {
		return fe.message, fe.code, true
	}
	return "", "", false
}

type fieldError struct {
	code    string
	message string
}

func (e *fieldError) Error() string { return e.message }

func badDraft(code, message string) error { return &fieldError{code: code, message: message} }

// applyDraft writes every part of a draft through the same queries the
// individual endpoints use, on the transaction it is given.
//
// The order matters only for readability; the transaction makes it atomic.
func (a *AdminHandler) applyDraft(
	ctx context.Context,
	q *sqlc.Queries,
	tenantID uuid.UUID,
	sf *Storefront,
	doc draftDocument,
) error {
	id := pgutil.UUID(tenantID)

	if s := doc.Store; s != nil {
		var nameParam pgtype.Text
		if s.Name != nil {
			name := trimText(*s.Name, 80)
			if utf8Count(name) < 2 {
				return badDraft("invalid_name", "Your business name needs at least 2 characters")
			}
			nameParam = pgutil.Text(name)
		}
		if _, err := q.UpdateStorefrontIdentity(ctx, sqlc.UpdateStorefrontIdentityParams{
			BusinessName: nameParam,
			LogoUrl:      trimmedText(s.LogoURL, 500),
			FaviconUrl:   trimmedText(s.FaviconURL, 500),
			Tagline:      trimmedText(s.Tagline, 140),
			Description:  trimmedText(s.Description, 500),
			Phone:        trimmedText(s.Phone, 40),
			Address:      trimmedText(s.Address, 300),
			TenantID:     id,
		}); err != nil {
			return err
		}
	}

	if t := doc.Theme; t != nil {
		// The same validation the direct endpoint runs. A draft is untrusted
		// input, and one that has been sitting in a table for a week is no more
		// trustworthy than one that just arrived.
		params, err := themeParams(themeRequest{
			Preset: t.Preset, Mode: t.Mode, Font: t.Font, Radius: t.Radius,
			Button: t.Button, Card: t.Card, Header: t.Header, Hero: t.Hero,
			Layout: t.ProductLayout, Filter: t.FilterStyle,
			Primary: t.Primary, Secondary: t.Secondary, Accent: t.Accent,
			HeroImageURL: t.HeroImageURL, CustomerModeSwitch: t.CustomerModeSwitch,
		}, tenantID)
		if err != nil {
			return err
		}
		if _, err := q.UpdateStorefrontTheme(ctx, params); err != nil {
			return err
		}
	}

	if b := doc.Behaviour; b != nil {
		if b.OrderingEnabled != nil || b.ClosedMessage != nil || b.CustomerLoginMode != nil || b.PrepTimeMinutes != nil {
			minutes := sf.PrepTimeMinutes
			if b.PrepTimeMinutes != nil {
				if *b.PrepTimeMinutes < 0 || *b.PrepTimeMinutes > 240 {
					return badDraft("invalid_prep_time", "Preparation time must be between 0 and 240 minutes")
				}
				minutes = *b.PrepTimeMinutes
			}
			params := sqlc.UpdateStorefrontBehaviourParams{
				OrderingEnabled: boolPtr(b.OrderingEnabled),
				ClosedMessage:   trimmedText(b.ClosedMessage, 200),
				PrepTimeMinutes: int32Ptr(&minutes),
				TenantID:        id,
			}
			if b.CustomerLoginMode != nil {
				mode := strings.ToLower(strings.TrimSpace(*b.CustomerLoginMode))
				if !contains(validLoginModes, mode) {
					return badDraft("invalid_login_mode", "Login must be off, optional or required")
				}
				params.CustomerLoginMode = pgutil.NullText(&mode)
			}
			if _, err := q.UpdateStorefrontBehaviour(ctx, params); err != nil {
				return err
			}
		}

		if b.TaxPercent != nil || b.PackagingFee != nil {
			var taxParam, feeParam pgtype.Numeric
			if b.TaxPercent != nil {
				tax, okRate := parseAnyRate(b.TaxPercent)
				if !okRate {
					return badDraft("invalid_amount", "Tax percent must be a plain number")
				}
				num, err := pgutil.NumericFromFloat(tax)
				if err != nil {
					return badDraft("invalid_amount", "Tax percent could not be converted")
				}
				taxParam = num
			}
			if b.PackagingFee != nil {
				fee, okRate := parseAnyRate(b.PackagingFee)
				if !okRate {
					return badDraft("invalid_amount", "Packaging fee must be a plain number")
				}
				num, err := pgutil.NumericFromFloat(fee)
				if err != nil {
					return badDraft("invalid_amount", "Packaging fee could not be converted")
				}
				feeParam = num
			}
			if _, err := q.UpdateStorefrontCosting(ctx, sqlc.UpdateStorefrontCostingParams{
				TaxPercent:   taxParam,
				PackagingFee: feeParam,
				TenantID:     id,
			}); err != nil {
				return err
			}
		}

		if b.Published != nil {
			if _, err := q.SetStorefrontPublished(ctx, sqlc.SetStorefrontPublishedParams{
				IsPublished: *b.Published,
				ID:          id,
			}); err != nil {
				return err
			}
		}
		// store_status and status_message are live ops (Action / Customize save
		// them immediately). Publishing a Studio draft must not overwrite them.
	}

	// Opening hours are live ops — same as store status. Ignore any hours
	// still present on older draft documents.
	_ = doc.Hours

	if h := doc.Homepage; h != nil && len(h.Sections) > 0 {
		var sections []Section
		if err := json.Unmarshal(h.Sections, &sections); err != nil {
			return badDraft("invalid_layout", "That homepage layout could not be read")
		}
		// Unknown section types and content keys are dropped here, exactly as
		// they are on the direct endpoint, so a stale draft can never store
		// something the storefront cannot render.
		raw, err := json.Marshal(NormalizeHomepage(Homepage{Sections: sections}))
		if err != nil {
			return err
		}
		if _, err := q.UpdateStorefrontHomepage(ctx, sqlc.UpdateStorefrontHomepageParams{
			Homepage: raw,
			TenantID: id,
		}); err != nil {
			return err
		}
	}

	if len(doc.Payments) > 0 {
		next := parsePayments(doc.Payments)
		// A shop with no way to pay is not a shop.
		if !next.OnlineEnabled && !next.CashEnabled {
			return badDraft("no_payment_method", "Keep at least one payment method switched on")
		}
		if !next.Allows(next.DefaultMethod) {
			if next.OnlineEnabled {
				next.DefaultMethod = MethodOnline
			} else {
				next.DefaultMethod = MethodCash
			}
		}
		if _, err := q.UpdateStorefrontPayments(ctx, sqlc.UpdateStorefrontPaymentsParams{
			Payments: next.Marshal(),
			TenantID: id,
		}); err != nil {
			return err
		}
	}

	if len(doc.Workflow) > 0 {
		next := parseWorkflow(doc.Workflow)
		if _, err := q.UpdateStorefrontWorkflow(ctx, sqlc.UpdateStorefrontWorkflowParams{
			Workflow: next.Marshal(),
			TenantID: id,
		}); err != nil {
			return err
		}
	}

	return nil
}
