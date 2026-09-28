package storefront

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Defaults is the starting storefront configuration chosen when a business is
// onboarded. It exists because a business type is not a label: a grocery and a
// momo counter want different layouts, different login rules and a different
// order workflow on day one, and the person onboarding them should not have to
// hand the shop over half-configured.
//
// Every field is optional. A zero value means "leave the column at its schema
// default", so a caller that only wants to set the layout sets only the layout.
type Defaults struct {
	ThemePreset    string
	ThemeMode      string
	PrimaryColor   string
	SecondaryColor string
	AccentColor    string
	FilterStyle    string
	ProductLayout  string
	HeroStyle      string
	FontFamily     string
	Radius         string
	CardStyle      string
	ButtonStyle    string

	OrderingEnabled   *bool
	CustomerLoginMode string
	PrepTimeMinutes   *int

	// Payments and Workflow arrive as the documents the console sends. They are
	// normalised through the same parsers the storefront reads with, so an
	// unknown value becomes the safe default instead of a broken shop. The
	// order state machine lives in the orders package and cannot be touched
	// from here — only the behaviour around it.
	Payments json.RawMessage
	Workflow json.RawMessage
}

// empty reports whether the defaults would change nothing.
func (d Defaults) empty() bool {
	return d.ThemePreset == "" && d.ThemeMode == "" && d.PrimaryColor == "" &&
		d.SecondaryColor == "" && d.AccentColor == "" && d.FilterStyle == "" &&
		d.ProductLayout == "" && d.HeroStyle == "" &&
		d.FontFamily == "" && d.Radius == "" && d.CardStyle == "" && d.ButtonStyle == "" &&
		d.OrderingEnabled == nil && d.CustomerLoginMode == "" && d.PrepTimeMinutes == nil &&
		len(d.Payments) == 0 && len(d.Workflow) == 0
}

// Provision creates a tenant's storefront row and applies its business-type
// defaults. It takes a *sqlc.Queries rather than a pool so the caller can run
// it inside the transaction that creates the tenant: a business is either
// created fully configured or not created at all.
func Provision(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, seed Seed, d Defaults) error {
	id := pgutil.UUID(tenantID)

	if _, err := q.EnsureStorefrontSettings(ctx, sqlc.EnsureStorefrontSettingsParams{
		TenantID:     id,
		BusinessName: seed.Name,
		Phone:        seed.Phone,
		Address:      seed.Address,
		LogoUrl:      seed.LogoURL,
		FaviconUrl:   seed.FaviconURL,
	}); err != nil {
		return err
	}
	if d.empty() {
		return nil
	}

	theme := sqlc.UpdateStorefrontThemeParams{
		TenantID:    id,
		ThemePreset: enumText(d.ThemePreset, validPresets),
		ThemeMode:   enumText(d.ThemeMode, validModes),
		// Colours are free-form within a shape: a value that is not a hex
		// colour is dropped rather than stored, because it would reach a
		// customer's browser as a style.
		PrimaryColor:   hexText(d.PrimaryColor),
		SecondaryColor: hexText(d.SecondaryColor),
		AccentColor:    hexText(d.AccentColor),
		FilterStyle:    enumText(d.FilterStyle, validFilters),
		ProductLayout:  enumText(d.ProductLayout, validLayouts),
		HeroStyle:      enumText(d.HeroStyle, validHeros),
		FontFamily:     enumText(d.FontFamily, validFonts),
		Radius:         enumText(d.Radius, validRadii),
		CardStyle:      enumText(d.CardStyle, validCards),
		ButtonStyle:    enumText(d.ButtonStyle, validButtons),
	}
	if themeTouched(theme) {
		if _, err := q.UpdateStorefrontTheme(ctx, theme); err != nil {
			return err
		}
	}

	behaviour := sqlc.UpdateStorefrontBehaviourParams{
		TenantID:          id,
		CustomerLoginMode: enumText(d.CustomerLoginMode, validLoginModes),
	}
	if d.OrderingEnabled != nil {
		behaviour.OrderingEnabled = pgtype.Bool{Bool: *d.OrderingEnabled, Valid: true}
	}
	if d.PrepTimeMinutes != nil {
		// Bounded for the same reason the admin screen bounds it: a prep time
		// of zero or of a week is a typo, not a policy.
		mins := *d.PrepTimeMinutes
		if mins < 1 {
			mins = 1
		}
		if mins > 240 {
			mins = 240
		}
		behaviour.PrepTimeMinutes = pgtype.Int4{Int32: int32(mins), Valid: true}
	}
	if behaviour.OrderingEnabled.Valid || behaviour.CustomerLoginMode.Valid || behaviour.PrepTimeMinutes.Valid {
		if _, err := q.UpdateStorefrontBehaviour(ctx, behaviour); err != nil {
			return err
		}
	}

	if len(d.Payments) > 0 {
		if _, err := q.UpdateStorefrontPayments(ctx, sqlc.UpdateStorefrontPaymentsParams{
			TenantID: id,
			Payments: parsePayments(d.Payments).Marshal(),
		}); err != nil {
			return err
		}
	}

	if len(d.Workflow) > 0 {
		if _, err := q.UpdateStorefrontWorkflow(ctx, sqlc.UpdateStorefrontWorkflowParams{
			TenantID: id,
			Workflow: parseWorkflow(d.Workflow).Marshal(),
		}); err != nil {
			return err
		}
	}

	return nil
}

// enumText returns a settable column value only when the caller supplied a
// value the schema accepts. An unrecognised value is dropped rather than
// substituted, so a typo in a template leaves the schema default in place
// instead of quietly picking something else.
func enumText(value string, allowed []string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	normalized := oneOf(value, "", allowed)
	if normalized == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: normalized, Valid: true}
}

func themeTouched(p sqlc.UpdateStorefrontThemeParams) bool {
	return p.ThemePreset.Valid || p.ThemeMode.Valid || p.PrimaryColor.Valid ||
		p.SecondaryColor.Valid || p.AccentColor.Valid || p.FilterStyle.Valid ||
		p.ProductLayout.Valid || p.HeroStyle.Valid ||
		p.FontFamily.Valid || p.Radius.Valid || p.CardStyle.Valid || p.ButtonStyle.Valid
}

// hexText accepts a six-digit hex colour and nothing else.
func hexText(value string) pgtype.Text {
	v := strings.TrimSpace(value)
	if len(v) != 7 || v[0] != '#' {
		return pgtype.Text{}
	}
	for _, r := range v[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return pgtype.Text{}
		}
	}
	return pgtype.Text{String: strings.ToLower(v), Valid: true}
}
