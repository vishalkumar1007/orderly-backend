package storefront

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestThemePresetsAreAllValidAndDistinct(t *testing.T) {
	presets := Presets()
	if len(presets) != 6 {
		t.Fatalf("expected 6 presets, got %d", len(presets))
	}
	seen := map[string]bool{}
	for _, p := range presets {
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true

		if p.Name == "" || p.Blurb == "" {
			t.Errorf("preset %s is missing a name or blurb", p.ID)
		}
		for label, colour := range map[string]string{
			"primary": p.Primary, "secondary": p.Secondary, "accent": p.Accent,
		} {
			if _, ok := parseHex(colour); !ok {
				t.Errorf("preset %s has an unparseable %s colour %q", p.ID, label, colour)
			}
		}
		if !contains(validPresets, p.ID) {
			t.Errorf("preset %s is not in the allowed enum", p.ID)
		}
		if !contains(validModes, p.Mode) {
			t.Errorf("preset %s has an invalid mode %q", p.ID, p.Mode)
		}
		if !contains(validRadii, p.Radius) {
			t.Errorf("preset %s has an invalid radius %q", p.ID, p.Radius)
		}
		if !contains(validButtons, p.ButtonStyle) {
			t.Errorf("preset %s has an invalid button style %q", p.ID, p.ButtonStyle)
		}
		if !contains(validCards, p.CardStyle) {
			t.Errorf("preset %s has an invalid card style %q", p.ID, p.CardStyle)
		}
		if !contains(validHeaders, p.HeaderStyle) {
			t.Errorf("preset %s has an invalid header style %q", p.ID, p.HeaderStyle)
		}
		if !contains(validHeros, p.HeroStyle) {
			t.Errorf("preset %s has an invalid hero style %q", p.ID, p.HeroStyle)
		}
	}
}

func TestEveryPresetProducesCompleteTokens(t *testing.T) {
	required := []string{
		"--sf-primary", "--sf-primary-ink", "--sf-primary-soft", "--sf-primary-rgb",
		"--sf-secondary", "--sf-accent", "--sf-radius", "--sf-radius-lg",
		"--sf-btn-radius", "--sf-font", "--sf-card-shadow", "--sf-header-bg",
		"--sf-hero-gradient", "--sf-focus-ring",
	}
	for _, p := range Presets() {
		theme := Theme{
			Preset: p.ID, Mode: p.Mode, Font: p.Font, Radius: p.Radius,
			Button: p.ButtonStyle, Card: p.CardStyle, Header: p.HeaderStyle,
			Hero: p.HeroStyle, Primary: p.Primary, Secondary: p.Secondary, Accent: p.Accent,
		}
		vars := theme.CSSVars()
		for _, key := range required {
			if vars[key] == "" {
				t.Errorf("preset %s is missing token %s", p.ID, key)
			}
		}
		if list := theme.VarList(); !strings.Contains(list, "--sf-primary:") {
			t.Errorf("preset %s did not serialise to a style string", p.ID)
		}
	}
}

func TestContrastInkIsReadableOnEveryBrandColour(t *testing.T) {
	// The ink colour is computed rather than hard-coded so a tenant that picks a
	// yellow brand still gets legible button labels.
	for _, background := range []string{
		"#ffffff", "#000000", "#facc15", "#4a044e", "#5b4bdb", "#059669", "#f97316",
	} {
		ink := ContrastInk(background)
		ratio := contrastRatio(ink, background)
		if ratio < 4.5 {
			t.Errorf("ink %s on %s has contrast %.2f, below the 4.5 readability floor",
				ink, background, ratio)
		}
	}
}

func TestRadiusScaleIsMonotonic(t *testing.T) {
	// A larger radius choice must never produce a smaller component.
	order := []string{RadiusNone, RadiusSm, RadiusMd, RadiusLg, RadiusPill}
	prev := -1.0
	for _, name := range order {
		scale, ok := radiusScale[name]
		if !ok {
			t.Fatalf("radius %q has no scale", name)
		}
		size := parsePixels(t, scale[2])
		if size < prev {
			t.Errorf("radius %q (%v) is smaller than the previous step", name, scale[2])
		}
		prev = size
	}
}

func parsePixels(t *testing.T, value string) float64 {
	t.Helper()
	trimmed := strings.TrimSuffix(value, "px")
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return f
}

func TestHexOrRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"#ABCDEF":             "#abcdef",
		"#abc":                "#aabbcc",
		"not-":                "#5b4bdb",
		"":                    "#5b4bdb",
		"javascript:alert(1)": "#5b4bdb",
		"#12345":              "#5b4bdb",
	}
	for input, want := range cases {
		if got := hexOr(input, "#5b4bdb"); got != want {
			t.Errorf("hexOr(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSoftenLightensTowardsWhite(t *testing.T) {
	base, _ := parseHex("#000000")
	lighter, _ := parseHex(Soften("#000000", 0.5))
	if lighter.R <= base.R {
		t.Errorf("Soften did not lighten: %q", Soften("#000000", 0.5))
	}
	white, _ := parseHex(Soften("#000000", 1))
	if white.R != 255 || white.G != 255 || white.B != 255 {
		t.Errorf("Soften at 1.0 should be white, got %q", Soften("#000000", 1))
	}
}

func TestDefaultHomepageShape(t *testing.T) {
	home := DefaultHomepage()
	if len(home.Sections) != 9 {
		t.Fatalf("expected 9 default sections, got %d", len(home.Sections))
	}
	if _, found := home.Find(SectionHero); !found {
		t.Error("the default layout should include a hero")
	}
	// Announcement ships disabled: an empty bar at the top of every store would
	// be noise.
	announcement, _ := home.Find(SectionAnnouncement)
	if announcement.Enabled {
		t.Error("the announcement section should ship disabled")
	}
	if len(home.EnabledSections()) != 8 {
		t.Errorf("expected 8 enabled default sections, got %d", len(home.EnabledSections()))
	}
}

func TestNormalizeHomepageDropsUnknownSections(t *testing.T) {
	in := Homepage{Sections: []Section{
		{Type: SectionHero, Enabled: true, Content: map[string]string{"title": "Hi"}},
		{Type: "CUSTOM_HTML", Enabled: true, Content: map[string]string{"html": "<script>x</script>"}},
		{Type: SectionMenu, Enabled: true},
	}}
	out := NormalizeHomepage(in)
	for _, s := range out.Sections {
		if s.Type == "CUSTOM_HTML" {
			t.Fatal("an unknown section type must not survive normalisation")
		}
	}
	if len(out.Sections) != 2 {
		t.Errorf("expected 2 sections, got %d", len(out.Sections))
	}
}

func TestNormalizeHomepageStripsUndeclaredContentKeys(t *testing.T) {
	in := Homepage{Sections: []Section{{
		Type:    SectionHero,
		Enabled: true,
		Content: map[string]string{
			"title":      "Welcome",
			"onclick":    "alert(1)",
			"style":      "position:fixed",
			"inner_html": "<b>hi</b>",
		},
	}}}
	out := NormalizeHomepage(in)
	hero, ok := out.Find(SectionHero)
	if !ok {
		t.Fatal("hero section went missing")
	}
	if hero.Content["title"] != "Welcome" {
		t.Errorf("declared content was lost: %+v", hero.Content)
	}
	for _, key := range []string{"onclick", "style", "inner_html"} {
		if _, present := hero.Content[key]; present {
			t.Errorf("undeclared key %q survived normalisation", key)
		}
	}
}

func TestNormalizeHomepageTruncatesOverlongContent(t *testing.T) {
	long := strings.Repeat("a", 500)
	out := NormalizeHomepage(Homepage{Sections: []Section{{
		Type: SectionAnnouncement, Enabled: true, Content: map[string]string{"text": long},
	}}})
	section, _ := out.Find(SectionAnnouncement)
	if len(section.Content["text"]) > 160 {
		t.Errorf("announcement text should be capped at 160 chars, got %d", len(section.Content["text"]))
	}
}

func TestNormalizeHomepageDisablesEmptyAnnouncement(t *testing.T) {
	out := NormalizeHomepage(Homepage{Sections: []Section{{
		Type: SectionAnnouncement, Enabled: true, Content: map[string]string{"text": "  "},
	}}})
	section, _ := out.Find(SectionAnnouncement)
	if section.Enabled {
		t.Error("an announcement with no message should be disabled")
	}
}

func TestNormalizeHomepageRejectsNonHTTPHeroImage(t *testing.T) {
	out := NormalizeHomepage(Homepage{Sections: []Section{{
		Type: SectionHero, Enabled: true,
		Content: map[string]string{"image_url": "javascript:alert(1)"},
	}}})
	section, _ := out.Find(SectionHero)
	if _, present := section.Content["image_url"]; present {
		t.Error("a non-http hero image must be dropped")
	}
}

func TestNormalizeHomepageFallsBackToDefaultWhenEmpty(t *testing.T) {
	out := NormalizeHomepage(Homepage{})
	if len(out.Sections) != len(DefaultHomepage().Sections) {
		t.Error("an empty layout should fall back to the default")
	}
}

func TestParsePaymentsRejectsDisablingEverything(t *testing.T) {
	doc := []byte(`{"online_payment_enabled":false,"cash_enabled":false}`)
	got := parsePayments(doc)
	if !got.OnlineEnabled && !got.CashEnabled {
		t.Error("a tenant must never end up with no payment method available")
	}
	if !got.Allows(got.DefaultMethod) {
		t.Errorf("default method %q is not enabled", got.DefaultMethod)
	}
}

func TestParsePaymentsRepairsAnIncoherentDefault(t *testing.T) {
	doc := []byte(`{"online_payment_enabled":false,"cash_enabled":true,"default_payment_method":"ONLINE"}`)
	got := parsePayments(doc)
	if got.DefaultMethod != MethodCash {
		t.Errorf("default = %q, want CASH once online is off", got.DefaultMethod)
	}
}

func TestParsePaymentsOnGarbageKeepsSafeDefaults(t *testing.T) {
	got := parsePayments([]byte("not json at all"))
	if !got.OnlineEnabled || !got.CashEnabled {
		t.Error("a corrupt payment document must fall back to both methods on")
	}
}

func TestParseWorkflowValidatesEnums(t *testing.T) {
	got := parseWorkflow([]byte(`{"acceptance_mode":"WHENEVER","payment_requirement":"WHENEVER"}`))
	if got.AcceptanceMode != AcceptManual {
		t.Errorf("acceptance = %q, want MANUAL", got.AcceptanceMode)
	}
	if got.PaymentRequirement != PayBeforePrep {
		t.Errorf("payment requirement = %q, want BEFORE_PREPARATION", got.PaymentRequirement)
	}
}

func TestWorkflowHelpers(t *testing.T) {
	if !(Workflow{AcceptanceMode: AcceptAuto}).AutoAccept() {
		t.Error("AUTO should report AutoAccept")
	}
	if (Workflow{AcceptanceMode: AcceptManual}).AutoAccept() {
		t.Error("MANUAL must not auto accept")
	}
	if !(Workflow{PaymentRequirement: PayBeforePrep}).RequiresPaymentUpfront() {
		t.Error("BEFORE_PREPARATION should require payment upfront")
	}
	if (Workflow{PaymentRequirement: PayAtPickup}).RequiresPaymentUpfront() {
		t.Error("AT_PICKUP must not require payment upfront")
	}
}

func TestPaymentMethodsOrderAndFiltering(t *testing.T) {
	settings := PaymentSettings{OnlineEnabled: true, CashEnabled: false, DefaultMethod: MethodOnline}
	if got := settings.Methods(); len(got) != 1 || got[0] != MethodOnline {
		t.Errorf("methods = %v, want [ONLINE]", got)
	}
	if settings.Allows(MethodCash) {
		t.Error("cash should not be offered when it is switched off")
	}
	if settings.Allows("BITCOIN") {
		t.Error("an unknown method must never be allowed")
	}
}

func TestHoursAlwaysOpen(t *testing.T) {
	h := parseHours([]byte(`{"always_open":true}`), "Asia/Kolkata")
	for _, day := range []int{0, 3, 6} {
		moment := time.Date(2026, 3, day+1, 3, 30, 0, 0, time.UTC)
		if !h.IsOpen(moment) {
			t.Errorf("an always-open store should be open on day %d", day)
		}
	}
}

func TestHoursRespectsTheSchedule(t *testing.T) {
	doc := []byte(`{"always_open":false,"schedule":{"mon":[["09:00","12:00"]]}}`)
	h := parseHours(doc, "UTC")
	monday := func(hour, minute int) time.Time {
		// 2026-03-02 is a Monday.
		return time.Date(2026, 3, 2, hour, minute, 0, 0, time.UTC)
	}
	if !h.IsOpen(monday(10, 0)) {
		t.Error("10:00 on Monday should be open")
	}
	if h.IsOpen(monday(8, 59)) {
		t.Error("08:59 on Monday should be closed")
	}
	if h.IsOpen(monday(12, 0)) {
		t.Error("12:00 is the closing time and should be closed")
	}
	// Tuesday has no entry, so the store is shut.
	if h.IsOpen(time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)) {
		t.Error("Tuesday has no hours and should be closed")
	}
}

func TestHoursAcceptsBothScheduleShapes(t *testing.T) {
	flat := parseHours([]byte(`{"always_open":false,"schedule":{"mon":["09:00","17:00"]}}`), "UTC")
	split := parseHours([]byte(`{"always_open":false,"schedule":{"mon":[["09:00","17:00"]]}}`), "UTC")
	if len(flat.Schedule["mon"]) != 1 || len(split.Schedule["mon"]) != 1 {
		t.Fatalf("both shapes should normalise to one range: %v vs %v", flat.Schedule, split.Schedule)
	}
	// 2026-03-02 is a Monday; midday falls inside both normalisations.
	probe := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	if !flat.IsOpen(probe) || !split.IsOpen(probe) {
		t.Error("both shapes should mark the same window open")
	}
}

func TestHoursDropsMalformedRanges(t *testing.T) {
	doc := []byte(`{"always_open":false,"schedule":{"mon":[["9am","17:00"],["25:00","26:00"],["18:00","07:00"]]}}`)
	h := parseHours(doc, "UTC")
	if len(h.Schedule["mon"]) != 0 {
		t.Errorf("no range here is valid, got %v", h.Schedule["mon"])
	}
	// A schedule with nothing usable must not read as "never open", which would
	// silently kill ordering.
	if !h.AlwaysOpen {
		t.Error("an unusable schedule should fall back to always open")
	}
}

func TestHoursStatusLabels(t *testing.T) {
	doc := []byte(`{"always_open":false,"schedule":{"mon":[["09:00","17:00"]]}}`)
	h := parseHours(doc, "UTC")
	monday := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	status := h.Status(monday)
	if !status.Open || status.Label != "Open" {
		t.Errorf("status = %+v, want open", status)
	}
	if !strings.Contains(status.Detail, "17:00") {
		t.Errorf("an open status should say when it closes, got %q", status.Detail)
	}
	closed := h.Status(time.Date(2026, 3, 2, 20, 0, 0, 0, time.UTC))
	if closed.Open || closed.Label != "Closed" {
		t.Errorf("status = %+v, want closed", closed)
	}
}

func TestHoursUsesTheTenantTimezone(t *testing.T) {
	// 18:00 UTC is 23:30 in Kolkata, so a shop open 09:00–22:00 local should be
	// shut even though it is late afternoon in UTC.
	doc := []byte(`{"always_open":false,"schedule":{"mon":[["09:00","22:00"]]}}`)
	h := parseHours(doc, "Asia/Kolkata")
	utcMonday := time.Date(2026, 3, 2, 18, 0, 0, 0, time.UTC)
	if h.IsOpen(utcMonday) {
		t.Error("18:00 UTC is 23:30 in Kolkata, which is outside 09:00-22:00")
	}
	if !h.IsOpen(utcMonday.Add(-5 * time.Hour)) {
		t.Error("13:00 UTC is 18:30 in Kolkata, which is inside the window")
	}
}

func TestParseAddonsSanitisesEntries(t *testing.T) {
	raw := []byte(`[
	  {"id":"spicy","name":"Extra spicy","price":10,"max_qty":1},
	  {"name":"Extra sauce","price":20},
	  {"name":"","price":5},
	  {"name":"Free treat","price":-5},
	  {"id":"too_many","name":"Overflow","price":5,"max_qty":999}
	]`)
	addons := ParseAddons(raw)
	if len(addons) != 3 {
		t.Fatalf("expected 3 usable add-ons, got %d: %+v", len(addons), addons)
	}
	if addons[1].ID != "extra-sauce" {
		t.Errorf("a missing id should be derived from the name, got %q", addons[1].ID)
	}
	if addons[1].MaxQty != 1 {
		t.Errorf("max_qty should default to 1, got %d", addons[1].MaxQty)
	}
	if addons[2].MaxQty != 20 {
		t.Errorf("max_qty should be capped at 20, got %d", addons[2].MaxQty)
	}
}

func TestParseAddonsOnGarbage(t *testing.T) {
	if got := ParseAddons([]byte("{{{not json")); len(got) != 0 {
		t.Errorf("expected no add-ons from garbage, got %+v", got)
	}
	if got := ParseAddons(nil); len(got) != 0 {
		t.Errorf("expected no add-ons from nil, got %+v", got)
	}
}

func TestProductFindAddon(t *testing.T) {
	p := Product{Addons: ParseAddons([]byte(`[{"id":"spicy","name":"Extra spicy","price":10}]`))}
	if _, ok := p.FindAddon("spicy"); !ok {
		t.Error("expected to find a declared add-on")
	}
	if _, ok := p.FindAddon("nope"); ok {
		t.Error("must not find an undeclared add-on")
	}
}

func TestReferenceInitials(t *testing.T) {
	cases := map[string]string{
		"Momo Magic": "MM",
		"gpg":        "GP",
		"The Corner": "TC",
		"a":          "A",
		"123 street": "ST",
		"":           "",
	}
	for input, want := range cases {
		if got := ReferenceInitials(input); got != want {
			t.Errorf("ReferenceInitials(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestOrderReference(t *testing.T) {
	sf := Storefront{Name: "Momo Magic", Slug: "momo-magic"}
	if got := sf.OrderReference(1024); got != "MM1024" {
		t.Errorf("OrderReference = %q, want MM1024", got)
	}
	// A tenant with no letters at all still gets a stable reference rather than
	// a bare number.
	bare := Storefront{Name: "123", Slug: "123"}
	if got := bare.OrderReference(7); got != "OR0007" {
		t.Errorf("OrderReference = %q, want OR0007", got)
	}
}

func TestSafeImageURLRejectsDangerousSchemes(t *testing.T) {
	cases := map[string]string{
		"https://cdn.example.com/a.jpg": "https://cdn.example.com/a.jpg",
		"http://cdn.example.com/a.jpg":  "http://cdn.example.com/a.jpg",
		"javascript:alert(1)":           "",
		"data:image/svg+xml;base64,xx":  "",
		"//cdn.example.com/a.jpg":       "",
		"/relative.jpg":                 "",
		"":                              "",
	}
	for input, want := range cases {
		if got := safeImageURL(input); got != want {
			t.Errorf("safeImageURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSectionContentOr(t *testing.T) {
	s := Section{Content: map[string]string{"title": "  ", "subtitle": "Fresh daily"}}
	if got := s.ContentOr("title", "fallback"); got != "fallback" {
		t.Errorf("blank content should fall back, got %q", got)
	}
	if got := s.ContentOr("subtitle", "fallback"); got != "Fresh daily" {
		t.Errorf("got %q", got)
	}
	if got := s.ContentOr("missing", "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
}

func TestSectionTypesCoverEveryDefaultSection(t *testing.T) {
	known := map[string]bool{}
	for _, def := range SectionTypes() {
		known[def.Type] = true
	}
	for _, s := range DefaultHomepage().Sections {
		if !known[s.Type] {
			t.Errorf("default section %q has no catalogue entry, so the builder could not render it", s.Type)
		}
	}
}

func TestBuildQRCodeIsReadableAtAnyBrandColour(t *testing.T) {
	for _, dark := range []string{"#fde047", "#ffffff", "#5b4bdb", "#000000", "not-a-colour"} {
		out, err := BuildQRCode("https://momo-magic.example.com", QRCodeOptions{Dark: dark})
		if err != nil {
			t.Fatalf("BuildQRCode(%q): %v", dark, err)
		}
		if !strings.HasPrefix(out.PNG, "data:image/png;base64,") {
			t.Errorf("PNG should be a data URI, got %.40q", out.PNG)
		}
		if !strings.HasPrefix(out.SVG, "<svg") {
			t.Errorf("SVG should be inline vector markup")
		}
	}
}

func TestStorefrontURL(t *testing.T) {
	if got := StorefrontURL("Momo-Magic", "example.com", "https"); got != "https://momo-magic.example.com" {
		t.Errorf("got %q", got)
	}
	if got := StorefrontURL("  ", "example.com", "https"); got != "" {
		t.Errorf("an empty slug should produce no URL, got %q", got)
	}
}

func TestQRDownloadName(t *testing.T) {
	if got := QRDownloadName("Momo Magic"); got != "orderly-momo-magic-qr.png" {
		t.Errorf("got %q", got)
	}
	if got := QRDownloadName(""); got != "orderly-store-qr.png" {
		t.Errorf("got %q", got)
	}
}

func TestMenuHelpers(t *testing.T) {
	menu := Menu{
		Categories: []Category{{ID: "c1", Name: "Momo", Products: []Product{{ID: "p1"}}}},
		Products:   []Product{{ID: "p1"}, {ID: "p2"}},
	}
	if _, ok := menu.Product("p2"); !ok {
		t.Error("expected to find p2")
	}
	if _, ok := menu.Product("ghost"); ok {
		t.Error("must not find a product that is not there")
	}
	if got := menu.ProductsIn("c1"); len(got) != 1 {
		t.Errorf("ProductsIn(c1) = %d items, want 1", len(got))
	}
	if got := menu.ProductsIn(""); len(got) != 2 {
		t.Errorf("ProductsIn(\"\") should return every product, got %d", len(got))
	}
	if got := menu.ProductsIn("ghost"); got != nil {
		t.Errorf("an unknown category should return nothing, got %+v", got)
	}
}

func TestEstimatedReady(t *testing.T) {
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	if got := EstimatedReady(20, now); !got.Equal(now.Add(20 * time.Minute)) {
		t.Errorf("got %v", got)
	}
	// A negative prep time must not produce a time in the past.
	if got := EstimatedReady(-5, now); !got.Equal(now) {
		t.Errorf("got %v", got)
	}
}
