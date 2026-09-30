package storefront

import (
	"fmt"
	"math"
	"strings"

	"github.com/orderly/orderly-backend/db/sqlc"
)

// Theme is the resolved, controlled design-token set for a storefront.
//
// There is deliberately no escape hatch: a tenant picks a preset, optionally
// overrides three colours, and chooses from fixed component-style enums. There
// is no field anywhere in this package that accepts arbitrary CSS, HTML or JS.
// Everything the client needs arrives as a flat map of CSS custom properties,
// so a storefront component can only read tokens the server chose.
type Theme struct {
	Preset    string
	Mode      string
	Font      string
	Radius    string
	Button    string
	Card      string
	Header    string
	Hero      string
	Layout    string
	Filter    string
	Primary   string
	Secondary string
	Accent    string
	// CustomerModeSwitch lets shoppers override light/dark on the storefront.
	CustomerModeSwitch bool
}

// PresetDef is one entry in the platform theme catalogue.
type PresetDef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Blurb       string `json:"blurb"`
	Primary     string `json:"primary"`
	Secondary   string `json:"secondary"`
	Accent      string `json:"accent"`
	Font        string `json:"font"`
	Radius      string `json:"radius"`
	ButtonStyle string `json:"button_style"`
	CardStyle   string `json:"card_style"`
	HeaderStyle string `json:"header_style"`
	HeroStyle   string `json:"hero_style"`
	Mode        string `json:"mode"`
}

// Presets is the platform-wide storefront theme catalogue. It is code, not
// configuration: a tenant can select one and override its three colours, but
// cannot introduce a new preset.
func Presets() []PresetDef {
	return []PresetDef{
		{
			ID: PresetClassic, Name: "Classic", Blurb: "Warm red, friendly and familiar",
			Primary: "#d7263d", Secondary: "#8c1c2b", Accent: "#f2b705",
			Font: FontInter, Radius: RadiusMd, ButtonStyle: ButtonRounded,
			CardStyle: CardElevated, HeaderStyle: HeaderSticky, HeroStyle: HeroImage,
			Mode: ModeLight,
		},
		{
			ID: PresetModern, Name: "Modern", Blurb: "Indigo and violet, clean geometry",
			Primary: "#5b4bdb", Secondary: "#8b5cf6", Accent: "#06b6d4",
			Font: FontInter, Radius: RadiusLg, ButtonStyle: ButtonSoft,
			CardStyle: CardElevated, HeaderStyle: HeaderSticky, HeroStyle: HeroGradient,
			Mode: ModeLight,
		},
		{
			ID: PresetStreet, Name: "Street Food", Blurb: "Bold orange, sharp corners",
			Primary: "#ea580c", Secondary: "#b91c1c", Accent: "#facc15",
			Font: FontSora, Radius: RadiusSm, ButtonStyle: ButtonSquare,
			CardStyle: CardOutlined, HeaderStyle: HeaderSolid, HeroStyle: HeroImage,
			Mode: ModeLight,
		},
		{
			ID: PresetMinimal, Name: "Minimal", Blurb: "Near-monochrome, quiet and calm",
			Primary: "#111827", Secondary: "#6b7280", Accent: "#9ca3af",
			Font: FontSystem, Radius: RadiusSm, ButtonStyle: ButtonSquare,
			CardStyle: CardMinimal, HeaderStyle: HeaderSolid, HeroStyle: HeroCompact,
			Mode: ModeLight,
		},
		{
			ID: PresetFresh, Name: "Fresh", Blurb: "Green, light and organic",
			Primary: "#059669", Secondary: "#0d9488", Accent: "#84cc16",
			Font: FontPoppins, Radius: RadiusLg, ButtonStyle: ButtonPill,
			CardStyle: CardFilled, HeaderStyle: HeaderSticky, HeroStyle: HeroGradient,
			Mode: ModeLight,
		},
		{
			ID: PresetDark, Name: "Dark", Blurb: "Charcoal base, neon accents",
			Primary: "#f97316", Secondary: "#fb923c", Accent: "#22d3ee",
			Font: FontSora, Radius: RadiusMd, ButtonStyle: ButtonRounded,
			CardStyle: CardElevated, HeaderStyle: HeaderSolid, HeroStyle: HeroGradient,
			Mode: ModeDark,
		},
	}
}

// PresetByID returns a catalogue entry, defaulting to Modern.
func PresetByID(id string) PresetDef {
	for _, p := range Presets() {
		if p.ID == id {
			return p
		}
	}
	return Presets()[1]
}

// FontStacks maps the font enum to a CSS font stack. Only the first family of
// each stack is loaded from the web font CDN, so the client adds at most one
// extra request.
var FontStacks = map[string]string{
	FontInter:   "'Inter', system-ui, -apple-system, 'Segoe UI', sans-serif",
	FontSora:    "'Sora', 'Inter', system-ui, sans-serif",
	FontPoppins: "'Poppins', 'Inter', system-ui, sans-serif",
	FontSystem:  "system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif",
}

// FontImports maps the font enum to the Google Fonts families to load.
var FontImports = map[string]string{
	FontInter:   "Inter:wght@400;500;600;700",
	FontSora:    "Sora:wght@600;700;800",
	FontPoppins: "Poppins:wght@400;500;600;700",
}

func resolveTheme(row sqlc.GetStorefrontConfigRow) Theme {
	base := PresetByID(row.ThemePreset)
	return Theme{
		Preset:             base.ID,
		Mode:               oneOf(row.ThemeMode, base.Mode, validModes),
		CustomerModeSwitch: row.CustomerThemeSwitchEnabled,
		Font:      oneOf(row.FontFamily, base.Font, validFonts),
		Radius:    oneOf(row.Radius, base.Radius, validRadii),
		Button:    oneOf(row.ButtonStyle, base.ButtonStyle, validButtons),
		Card:      oneOf(row.CardStyle, base.CardStyle, validCards),
		Header:    oneOf(row.HeaderStyle, base.HeaderStyle, validHeaders),
		Hero:      oneOf(row.HeroStyle, base.HeroStyle, validHeros),
		Layout:    oneOf(row.ProductLayout, LayoutList, validLayouts),
		Filter:    oneOf(row.FilterStyle, FilterChips, validFilters),
		Primary:   hexOr(row.PrimaryColor, base.Primary),
		Secondary: hexOr(row.SecondaryColor, base.Secondary),
		Accent:    hexOr(row.AccentColor, base.Accent),
	}
}

// ---------------------------------------------------------------------------
// Token → CSS custom properties
// ---------------------------------------------------------------------------

// radiusScale maps the radius enum to the four-step scale every component
// reads. It is a fixed set of steps, never a tenant-typed pixel value.
var radiusScale = map[string][4]string{
	RadiusNone: {"0px", "0px", "0px", "0px"},
	RadiusSm:   {"4px", "6px", "8px", "12px"},
	RadiusMd:   {"6px", "10px", "14px", "20px"},
	RadiusLg:   {"10px", "16px", "22px", "30px"},
	RadiusPill: {"16px", "22px", "999px", "999px"},
}

var buttonRadius = map[string]string{
	ButtonRounded: "calc(var(--sf-radius-sm) * 1.5)",
	ButtonPill:    "999px",
	ButtonSquare:  "2px",
	ButtonSoft:    "var(--sf-radius-lg)",
}

// CSSVars renders the theme as CSS custom properties for the storefront root
// element. The storefront layout writes these as an inline style on its root
// node, which means the very first server-rendered frame is already themed —
// there is no flash of an unstyled or wrong-coloured storefront.
//
// Contrast pairs (on-primary text, on-accent text) are computed here rather
// than hard-coded in CSS, so a tenant that picks a yellow brand still gets
// legible button labels.
func (t Theme) CSSVars() map[string]string {
	primary := t.Primary
	secondary := t.Secondary
	accent := t.Accent
	scale := radiusScale[t.Radius]

	vars := map[string]string{
		"--sf-primary":         primary,
		"--sf-primary-rgb":     rgbTriplet(primary),
		"--sf-primary-ink":     ContrastInk(primary),
		"--sf-primary-soft":    Soften(primary, 0.12),
		"--sf-primary-softer":  Soften(primary, 0.05),
		"--sf-secondary":       secondary,
		"--sf-secondary-rgb":   rgbTriplet(secondary),
		"--sf-secondary-ink":   ContrastInk(secondary),
		"--sf-accent":          accent,
		"--sf-accent-rgb":      rgbTriplet(accent),
		"--sf-accent-ink":      ContrastInk(accent),
		"--sf-accent-soft":     Soften(accent, 0.14),
		"--sf-radius-xs":       scale[0],
		"--sf-radius-sm":       scale[1],
		"--sf-radius":          scale[2],
		"--sf-radius-lg":       scale[3],
		"--sf-radius-pill":     "999px",
		"--sf-btn-radius":      buttonRadius[t.Button],
		"--sf-font":            FontStacks[t.Font],
		"--sf-font-display":    FontStacks[t.Font],
		"--sf-font-weight":     fontWeight(t.Font),
		"--sf-btn-transform":   buttonTransform(t.Button),
		"--sf-card-shadow":     cardShadow(t.Card),
		"--sf-card-border":     cardBorder(t.Card),
		"--sf-card-bg":         "var(--sf-surface)",
		"--sf-header-position": headerPosition(t.Header),
		"--sf-header-bg":       headerBackground(t.Header),
		"--sf-header-ink":      headerInk(t.Header),
		"--sf-hero-min-height": heroMinHeight(t.Hero),
		"--sf-hero-radius":     heroRadius(t.Hero),
		"--sf-hero-gradient":   heroGradient(primary, secondary, accent),
		"--sf-hero-align":      heroAlign(t.Hero),
		"--sf-section-space":   sectionSpace(t.Hero),
		"--sf-image-fit":       imageFit(t.Card),
		"--sf-product-layout":  t.Layout,
		"--sf-filter-style":    t.Filter,
		"--sf-focus-ring":      "color-mix(in srgb, " + primary + " 35%, transparent)",
	}
	return vars
}

// VarList renders CSSVars as an ordered `prop:value;…` string suitable for an
// HTML `style` attribute. Order is irrelevant to CSS, so map iteration order is
// fine.
func (t Theme) VarList() string {
	vars := t.CSSVars()
	parts := make([]string, 0, len(vars))
	for k, v := range vars {
		parts = append(parts, k+":"+v)
	}
	return strings.Join(parts, ";")
}

func fontWeight(font string) string {
	if font == FontSora {
		return "600"
	}
	return "600"
}

func buttonTransform(style string) string {
	if style == ButtonSquare {
		return "none"
	}
	return "scale(0.97)"
}

func cardShadow(style string) string {
	switch style {
	case CardElevated:
		return "var(--sf-shadow-md)"
	case CardFilled:
		return "none"
	case CardOutlined:
		return "var(--sf-shadow-xs)"
	default:
		return "none"
	}
}

func cardBorder(style string) string {
	switch style {
	case CardOutlined:
		return "var(--sf-border-strong)"
	case CardElevated:
		return "transparent"
	case CardFilled:
		return "transparent"
	default:
		return "transparent"
	}
}

func imageFit(style string) string {
	if style == CardMinimal {
		return "cover"
	}
	return "cover"
}

func headerPosition(style string) string {
	if style == HeaderSticky {
		return "sticky"
	}
	return "relative"
}

func headerBackground(style string) string {
	if style == HeaderTransp {
		return "transparent"
	}
	return "var(--sf-surface)"
}

func headerInk(style string) string {
	if style == HeaderTransp {
		return "var(--sf-text)"
	}
	return "var(--sf-text)"
}

func heroMinHeight(style string) string {
	switch style {
	case HeroImage:
		return "230px"
	case HeroGradient:
		return "185px"
	case HeroCompact:
		return "auto"
	default:
		return "0px"
	}
}

func heroRadius(style string) string {
	if style == HeroCompact {
		return "var(--sf-radius-lg)"
	}
	return "0px"
}

func heroAlign(style string) string {
	if style == HeroImage {
		return "flex-end"
	}
	return "center"
}

func heroGradient(primary, secondary, accent string) string {
	return fmt.Sprintf(
		"linear-gradient(135deg, %s 0%%, %s 55%%, %s 100%%)",
		Soften(primary, 0.0), Soften(secondary, 0.08), Soften(accent, 0.16),
	)
}

func sectionSpace(hero string) string {
	if hero == HeroCompact {
		return "20px"
	}
	return "26px"
}

// ---------------------------------------------------------------------------
// Colour helpers
// ---------------------------------------------------------------------------

type rgb struct{ R, G, B float64 }

func parseHex(hex string) (rgb, bool) {
	s := strings.TrimSpace(hex)
	s = strings.TrimPrefix(s, "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return rgb{}, false
	}
	var out [3]float64
	for i := 0; i < 3; i++ {
		var v int
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			var d int
			switch {
			case c >= '0' && c <= '9':
				d = int(c - '0')
			case c >= 'a' && c <= 'f':
				d = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				d = int(c-'A') + 10
			default:
				return rgb{}, false
			}
			v = v*16 + d
		}
		out[i] = float64(v)
	}
	return rgb{out[0], out[1], out[2]}, true
}

// hexOr returns the tenant colour when it is a valid hex, else the fallback.
// A malformed colour can never break the storefront's palette.
func hexOr(value, fallback string) string {
	if c, ok := parseHex(value); ok {
		return fmt.Sprintf("#%02x%02x%02x", int(c.R), int(c.G), int(c.B))
	}
	if c, ok := parseHex(fallback); ok {
		return fmt.Sprintf("#%02x%02x%02x", int(c.R), int(c.G), int(c.B))
	}
	return "#5b4bdb"
}

func rgbTriplet(hex string) string {
	c, ok := parseHex(hex)
	if !ok {
		return "91, 75, 219"
	}
	return fmt.Sprintf("%d, %d, %d", int(c.R), int(c.G), int(c.B))
}

// Soften blends a colour towards white by amount (0..1).
func Soften(hex string, amount float64) string {
	c, ok := parseHex(hex)
	if !ok {
		return hex
	}
	blend := func(v float64) float64 { return v + (255-v)*amount }
	return fmt.Sprintf("#%02x%02x%02x", int(blend(c.R)), int(blend(c.G)), int(blend(c.B)))
}

// darken blends a colour towards black by amount (0..1).
func darken(hex string, amount float64) string {
	c, ok := parseHex(hex)
	if !ok {
		return hex
	}
	blend := func(v float64) float64 { return v * (1 - amount) }
	return fmt.Sprintf("#%02x%02x%02x", int(blend(c.R)), int(blend(c.G)), int(blend(c.B)))
}

// hexOf renders a parsed colour back to #rrggbb.
func hexOf(c rgb) string {
	return fmt.Sprintf("#%02x%02x%02x", int(c.R), int(c.G), int(c.B))
}

// luminance is the WCAG relative luminance of a colour.
func luminance(c rgb) float64 {
	lin := func(v float64) float64 {
		v = v / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// ContrastInk picks black or white text for a background, using the WCAG
// relative-luminance formula. Guarantees readable labels on any brand colour.
func ContrastInk(background string) string {
	c, ok := parseHex(background)
	if !ok {
		return "#ffffff"
	}
	l := luminance(c)
	contrastWhite := 1.05 / (l + 0.05)
	contrastBlack := (l + 0.05) / 0.05
	if contrastBlack > contrastWhite {
		return "#0b1020"
	}
	return "#ffffff"
}
