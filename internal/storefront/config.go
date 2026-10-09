package storefront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Enumerations. These are the only accepted values for the corresponding
// columns; the database CHECK constraints mirror them.
const (
	PresetClassic = "classic"
	PresetModern  = "modern"
	PresetStreet  = "street-food"
	PresetMinimal = "minimal"
	PresetFresh   = "fresh"
	PresetDark    = "dark"

	ModeLight  = "light"
	ModeDark   = "dark"
	ModeSystem = "system"

	FontInter   = "inter"
	FontSora    = "sora"
	FontPoppins = "poppins"
	FontSystem  = "system"

	RadiusNone = "none"
	RadiusSm   = "sm"
	RadiusMd   = "md"
	RadiusLg   = "lg"
	RadiusPill = "pill"

	ButtonRounded = "rounded"
	ButtonPill    = "pill"
	ButtonSquare  = "square"
	ButtonSoft    = "soft"

	CardElevated = "elevated"
	CardOutlined = "outlined"
	CardFilled   = "filled"
	CardMinimal  = "minimal"

	HeaderSticky = "sticky"
	HeaderSolid  = "solid"
	HeaderTransp = "transparent"

	HeroImage    = "image"
	HeroGradient = "gradient"
	HeroCompact  = "compact"
	HeroNone     = "none"

	LayoutList    = "list"
	LayoutGrid    = "grid"
	LayoutCompact = "compact"

	FilterChips = "chips"
	FilterPills = "pills"
	FilterRail  = "rail"

	LoginOff      = "off"
	LoginOptional = "optional"
	LoginRequired = "required"

	MethodOnline = "ONLINE"
	MethodCash   = "CASH"

	AcceptManual = "MANUAL"
	AcceptAuto   = "AUTO"

	PayBeforePrep = "BEFORE_PREPARATION"
	PayAtPickup   = "AT_PICKUP"

	// Store status values. OPEN and BUSY allow ordering; AWAY and CLOSED block it.
	StoreOpen   = "OPEN"
	StoreBusy   = "BUSY"
	StoreAway   = "AWAY"
	StoreClosed = "CLOSED"

	// Notification sound choices. Played client-side (Web Audio tones, no
	// audio files) — this only picks which one, never a file to host.
	SoundNone  = "NONE"
	SoundChime = "CHIME"
	SoundBell  = "BELL"
	SoundPing  = "PING"
)

// validStoreStatuses lists every accepted store_status value.
var validStoreStatuses = []string{StoreOpen, StoreBusy, StoreAway, StoreClosed}

// StoreStatusInfo describes one store status for the admin UI.
type StoreStatusInfo struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description"`
	AllowsOrder bool   `json:"allows_order"`
}

// StoreStatuses returns the catalogue of available store statuses.
func StoreStatuses() []StoreStatusInfo {
	return []StoreStatusInfo{
		{Value: StoreOpen, Label: "Open", Description: "Accepting orders normally", AllowsOrder: true},
		{Value: StoreBusy, Label: "Busy", Description: "Open but with longer wait times", AllowsOrder: true},
		{Value: StoreAway, Label: "Away", Description: "Temporarily not accepting orders", AllowsOrder: false},
		{Value: StoreClosed, Label: "Closed", Description: "Not accepting orders", AllowsOrder: false},
	}
}

// StoreStatusLabel returns a human-readable label for a store status value.
func StoreStatusLabel(status string) string {
	for _, s := range validStoreStatuses {
		if s == status {
			switch s {
			case StoreOpen:
				return "Open"
			case StoreBusy:
				return "Busy"
			case StoreAway:
				return "Away"
			case StoreClosed:
				return "Closed"
			}
		}
	}
	return "Unknown"
}

// StoreStatusAllowsOrder reports whether a status permits new orders.
func StoreStatusAllowsOrder(status string) bool {
	switch status {
	case StoreOpen, StoreBusy:
		return true
	default:
		return false
	}
}

var (
	validPresets   = []string{PresetClassic, PresetModern, PresetStreet, PresetMinimal, PresetFresh, PresetDark}
	validModes     = []string{ModeLight, ModeDark, ModeSystem}
	validFonts     = []string{FontInter, FontSora, FontPoppins, FontSystem}
	validRadii     = []string{RadiusNone, RadiusSm, RadiusMd, RadiusLg, RadiusPill}
	validButtons   = []string{ButtonRounded, ButtonPill, ButtonSquare, ButtonSoft}
	validCards     = []string{CardElevated, CardOutlined, CardFilled, CardMinimal}
	validHeaders   = []string{HeaderSticky, HeaderSolid, HeaderTransp}
	validHeros     = []string{HeroImage, HeroGradient, HeroCompact, HeroNone}
	validLayouts   = []string{LayoutList, LayoutGrid, LayoutCompact}
	validFilters   = []string{FilterChips, FilterPills, FilterRail}
	validLoginModes = []string{LoginOff, LoginOptional, LoginRequired}
	validAccept    = []string{AcceptManual, AcceptAuto}
	validPayTiming = []string{PayBeforePrep, PayAtPickup}
	validSounds    = []string{SoundNone, SoundChime, SoundBell, SoundPing}
)

// ErrNotFound is returned when a tenant has no storefront configuration yet.
var ErrNotFound = errors.New("storefront settings not found")

// Storefront is the resolved, validated configuration for one tenant. Every
// consumer works from this struct rather than raw JSON, so a malformed
// document can never reach a customer.
type Storefront struct {
	TenantID    uuid.UUID
	TenantName  string
	Slug        string
	BusinessTy  string
	Currency    string
	Timezone    string
	IsPublished   bool
	StoreStatus   string
	StatusMessage string

	LogoURL    string
	FaviconURL string
	Name       string
	Tagline    string
	About      string
	Phone      string
	Address    string

	Theme Theme

	// HeroImageURL is an optional hero background. It must be an absolute
	// http(s) URL; the storefront never renders tenant-authored markup.
	HeroImageURL string

	OrderingEnabled bool
	ClosedMessage   string
	// CustomerLoginMode is off | optional | required. CustomerLogin is the
	// legacy boolean (mode != off) kept for older clients.
	CustomerLoginMode string
	CustomerLogin     bool
	PrepTimeMinutes int
	TaxPercent      float64
	PackagingFee    float64
	OpeningHours    Hours
	Homepage        Homepage
	Payments        PaymentSettings
	Workflow        Workflow
	UpdatedAt       time.Time

	rawHomepage     []byte
	rawPayments     []byte
	rawWorkflow     []byte
	rawOpeningHours []byte
}

// PaymentSettings is the tenant's payment method configuration.
type PaymentSettings struct {
	OnlineEnabled  bool   `json:"online_payment_enabled"`
	CashEnabled    bool   `json:"cash_enabled"`
	AtPickupEnable bool   `json:"pay_at_pickup_enabled"`
	DefaultMethod  string `json:"default_payment_method"`
}

// Methods returns the payment methods a customer may choose, in display order.
func (p PaymentSettings) Methods() []string {
	out := make([]string, 0, 2)
	if p.OnlineEnabled {
		out = append(out, MethodOnline)
	}
	if p.CashEnabled {
		out = append(out, MethodCash)
	}
	return out
}

// Allows reports whether a method is enabled for this tenant.
func (p PaymentSettings) Allows(method string) bool {
	switch method {
	case MethodOnline:
		return p.OnlineEnabled
	case MethodCash:
		return p.CashEnabled
	}
	return false
}

// Workflow is the tenant's order pipeline configuration. It tunes behaviour
// around a fixed state machine; it cannot introduce new states or unsafe
// transitions.
type Workflow struct {
	AcceptanceMode     string `json:"acceptance_mode"`
	PaymentRequirement string `json:"payment_requirement"`
	ReadyNotification  bool   `json:"ready_notification"`
	AutoComplete       bool   `json:"auto_complete"`
	// NewOrderSound plays in the shop console when an order arrives.
	NewOrderSound string `json:"new_order_sound"`
	// OrderReadySound plays on the customer's tracking page when their order
	// becomes ready, gated by ReadyNotification.
	OrderReadySound string `json:"order_ready_sound"`
	// InAppNewOrder/InAppOrderReady gate the in-app notification feed
	// (internal/notify), independent of the sounds above — a tenant can mute
	// the sound and still want the bell, or the reverse.
	InAppNewOrder   bool `json:"in_app_new_order_enabled"`
	InAppOrderReady bool `json:"in_app_order_ready_enabled"`
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// Loader reads storefront configuration.
type Loader struct {
	q *sqlc.Queries
}

// NewLoader builds a Loader over a pool.
func NewLoader(pool *pgxpool.Pool) *Loader { return &Loader{q: sqlc.New(pool)} }

// NewLoaderFromQueries builds a Loader over an existing query set, so a
// configuration read can join an existing transaction.
func NewLoaderFromQueries(q *sqlc.Queries) *Loader { return &Loader{q: q} }

// Load returns the tenant's storefront configuration.
func (l *Loader) Load(ctx context.Context, tenantID uuid.UUID) (*Storefront, error) {
	row, err := l.q.GetStorefrontConfig(ctx, pgutil.UUID(tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return fromRow(row)
}

// Ensure returns the configuration, creating the row on first use from the
// tenant's own identity so a brand new tenant still gets a working storefront
// instead of a 404. tenantID always comes from the resolved request context.
func (l *Loader) Ensure(ctx context.Context, tenantID uuid.UUID, seed Seed) (*Storefront, error) {
	if _, err := l.q.EnsureStorefrontSettings(ctx, sqlc.EnsureStorefrontSettingsParams{
		TenantID:     pgutil.UUID(tenantID),
		BusinessName: seed.Name,
		Phone:        seed.Phone,
		Address:      seed.Address,
		LogoUrl:      seed.LogoURL,
		FaviconUrl:   seed.FaviconURL,
	}); err != nil {
		return nil, err
	}
	return l.Load(ctx, tenantID)
}

// Seed carries the tenant fields used to bootstrap a storefront row.
type Seed struct {
	Name       string
	Phone      string
	Address    string
	LogoURL    string
	FaviconURL string
}

func fromRow(row sqlc.GetStorefrontConfigRow) (*Storefront, error) {
	sf := &Storefront{
		TenantID:        uuid.UUID(row.TenantID.Bytes),
		TenantName:      row.TenantName,
		Slug:            row.Slug,
		BusinessTy:      row.BusinessType,
		Currency:        row.Currency,
		Timezone:        row.Timezone,
		IsPublished:     row.IsPublished,
		StoreStatus:     row.StoreStatus,
		StatusMessage:   row.StatusMessage,
		LogoURL:         row.LogoUrl,
		FaviconURL:      row.FaviconUrl,
		Name:            strings.TrimSpace(row.BusinessName),
		Tagline:         row.Tagline,
		About:           row.Description,
		Phone:           row.Phone,
		Address:         row.Address,
		OrderingEnabled:   row.OrderingEnabled,
		ClosedMessage:     row.ClosedMessage,
		CustomerLoginMode: oneOf(row.CustomerLoginMode, LoginOptional, validLoginModes),
		PrepTimeMinutes:   int(row.PrepTimeMinutes),
		TaxPercent:        pgutil.NumericToFloat(row.TaxPercent),
		PackagingFee:      pgutil.NumericToFloat(row.PackagingFee),
		rawHomepage:       row.Homepage,
		rawPayments:       row.Payments,
		rawWorkflow:       row.Workflow,
		rawOpeningHours:   row.OpeningHours,
		UpdatedAt:         row.UpdatedAt.Time,
	}
	sf.CustomerLogin = sf.CustomerLoginMode != LoginOff
	if sf.Name == "" {
		sf.Name = row.TenantName
	}
	if sf.Timezone == "" {
		sf.Timezone = "UTC"
	}
	sf.HeroImageURL = safeImageURL(row.HeroImageUrl)
	sf.Theme = resolveTheme(row)
	sf.Homepage = parseHomepage(row.Homepage)
	sf.Payments = parsePayments(row.Payments)
	sf.Workflow = parseWorkflow(row.Workflow)
	sf.OpeningHours = parseHours(row.OpeningHours, sf.Timezone)
	return sf, nil
}

// RawDocuments exposes the stored JSONB documents so the tenant admin editor
// loads exactly what is persisted.
func (s *Storefront) RawDocuments() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	add := func(key string, raw []byte) {
		if len(raw) > 0 {
			out[key] = json.RawMessage(raw)
		}
	}
	add("homepage", s.rawHomepage)
	add("payments", s.rawPayments)
	add("workflow", s.rawWorkflow)
	add("opening_hours", s.rawOpeningHours)
	return out
}

// LoginRequired reports whether guests must sign in before placing an order.
func (s *Storefront) LoginRequired() bool {
	return s.CustomerLoginMode == LoginRequired
}

// LoginAllowed reports whether phone OTP login screens and APIs are available.
func (s *Storefront) LoginAllowed() bool {
	return s.CustomerLoginMode != LoginOff
}

// OrderingAllowed reports whether the storefront should accept new orders.
// A closed or away store, an explicitly disabled switch or a closed
// opening-hours window all block ordering; browsing stays available.
func (s *Storefront) OrderingAllowed() bool {
	if !s.IsPublished {
		return false
	}
	if !StoreStatusAllowsOrder(s.StoreStatus) || !s.OrderingEnabled {
		return false
	}
	return s.OpeningHours.IsOpen(time.Now())
}

// ClosedReason explains why ordering is unavailable, or "" when it is allowed.
func (s *Storefront) ClosedReason() string {
	if !s.IsPublished {
		return "This store is not available right now."
	}
	if !StoreStatusAllowsOrder(s.StoreStatus) || !s.OrderingEnabled || !s.OpeningHours.IsOpen(time.Now()) {
		if msg := strings.TrimSpace(s.StatusMessage); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(s.ClosedMessage); msg != "" {
			return msg
		}
		switch s.StoreStatus {
		case StoreAway:
			return "Temporarily away — back soon"
		case StoreBusy:
			return "Currently busy — longer wait times"
		case StoreClosed:
			return "Currently Closed"
		default:
			return "Currently Closed"
		}
	}
	return ""
}

// StoreStatusLabel returns a human-readable label for the current store status.
func (s *Storefront) StoreStatusLabel() string {
	return StoreStatusLabel(s.StoreStatus)
}

// DisplayStatusMessage returns the customer-facing status message, falling back
// to a default based on the current status.
func (s *Storefront) DisplayStatusMessage() string {
	if msg := strings.TrimSpace(s.StatusMessage); msg != "" {
		return msg
	}
	switch s.StoreStatus {
	case StoreBusy:
		return "High demand right now — expect longer wait times."
	case StoreAway:
		return "We're temporarily away. Please check back soon."
	case StoreClosed:
		return "We're closed right now."
	default:
		return ""
	}
}

// OrderReference builds the short human code shown to customers, e.g. "MM1024"
// for a tenant whose initials are MM. Derived, never stored, so it stays
// stable for the life of the order.
func (s *Storefront) OrderReference(number int32) string {
	initials := ReferenceInitials(s.Name)
	if initials == "" {
		initials = ReferenceInitials(s.Slug)
	}
	if initials == "" {
		initials = "OR"
	}
	return fmt.Sprintf("%s%04d", initials, number)
}

// ReferenceInitials derives up to two initials from a business name: one per
// word where there are several ("Momo Magic" → MM), and the first two letters of
// the single word when there is only one ("gpg" → GP). The result is the prefix
// customers read at the counter, so it has to look like something a human would
// have written.
func ReferenceInitials(name string) string {
	fields := strings.Fields(strings.ToUpper(name))
	words := make([]string, 0, len(fields))
	for _, f := range fields {
		word := make([]rune, 0, len(f))
		for _, r := range f {
			if r >= 'A' && r <= 'Z' {
				word = append(word, r)
			}
		}
		if len(word) > 0 {
			words = append(words, string(word))
		}
	}
	if len(words) == 0 {
		return ""
	}
	if len(words) == 1 {
		runes := []rune(words[0])
		if len(runes) > 2 {
			runes = runes[:2]
		}
		return string(runes)
	}
	return string([]rune{[]rune(words[0])[0], []rune(words[1])[0]})
}

// safeImageURL accepts only absolute http(s) image URLs. A tenant-supplied
// value is rendered into a CSS background or an <img src>; anything else
// (javascript:, data:, protocol-relative) is discarded rather than sanitised
// at render time.
func safeImageURL(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) > 500 {
		return ""
	}
	lower := strings.ToLower(v)
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return v
	}
	return ""
}

// oneOf returns value when it is in allowed, otherwise fallback.
func oneOf(value, fallback string, allowed []string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	for _, a := range allowed {
		if v == a {
			return a
		}
	}
	return fallback
}

// upperOneOf is oneOf for enum values stored in upper case (payment methods,
// workflow modes).
func upperOneOf(value, fallback string, allowed []string) string {
	v := strings.ToUpper(strings.TrimSpace(value))
	for _, a := range allowed {
		if v == a {
			return a
		}
	}
	return fallback
}
