package storefront

import (
	"encoding/json"
	"strings"
)

// Homepage section types. The set is closed: a tenant admin may enable,
// disable, reorder and edit these, but cannot inject a section type the
// storefront does not know how to render.
const (
	SectionAnnouncement   = "ANNOUNCEMENT"
	SectionHero           = "HERO"
	SectionCategories     = "CATEGORIES"
	SectionPopular        = "POPULAR_PRODUCTS"
	SectionFeatured       = "FEATURED_PRODUCTS"
	SectionMenu           = "MENU"
	SectionBusinessInfo   = "BUSINESS_INFO"
	SectionOpeningHours   = "OPENING_HOURS"
	SectionFooter         = "FOOTER"
	SectionDefaultSection = "MENU"
)

// SectionTypeDef describes one section for the admin builder.
type SectionTypeDef struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Blurb string `json:"blurb"`
	// Fields lists the editable content keys, in render order. The builder
	// renders a control per key; the storefront reads the same keys.
	Fields []SectionField `json:"fields"`
}

// SectionField is one editable content key within a section.
type SectionField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"` // text | textarea | image | number
	Hint  string `json:"hint,omitempty"`
	Max   int    `json:"max,omitempty"`
}

// SectionTypes is the catalogue the homepage builder renders from.
func SectionTypes() []SectionTypeDef {
	return []SectionTypeDef{
		{
			Type: SectionAnnouncement, Label: "Announcement",
			Blurb: "A single-line bar at the very top, e.g. a free-delivery offer.",
			Fields: []SectionField{
				{Key: "text", Label: "Message", Type: "textarea", Max: 160, Hint: "Leave empty to hide."},
				{Key: "tone", Label: "Tone", Type: "text", Hint: "info | success | warn"},
			},
		},
		{
			Type: SectionHero, Label: "Hero",
			Blurb: "The first thing a customer sees. Falls back to your store name and tagline.",
			Fields: []SectionField{
				{Key: "title", Label: "Headline", Type: "text", Max: 60},
				{Key: "subtitle", Label: "Sub-line", Type: "text", Max: 120},
				{Key: "image_url", Label: "Background image URL", Type: "image"},
				{Key: "cta_label", Label: "Button label", Type: "text", Max: 28},
			},
		},
		{
			Type: SectionCategories, Label: "Categories",
			Blurb: "Horizontal scroller of your menu categories.",
			Fields: []SectionField{
				{Key: "title", Label: "Heading", Type: "text", Max: 40},
			},
		},
		{
			Type: SectionPopular, Label: "Popular products",
			Blurb: "Products you have marked as popular in your menu.",
			Fields: []SectionField{
				{Key: "title", Label: "Heading", Type: "text", Max: 40},
			},
		},
		{
			Type: SectionFeatured, Label: "Featured products",
			Blurb: "Products you have marked as featured in your menu.",
			Fields: []SectionField{
				{Key: "title", Label: "Heading", Type: "text", Max: 40},
			},
		},
		{
			Type: SectionMenu, Label: "Full menu",
			Blurb: "Every category with all of its products, searchable.",
			Fields: []SectionField{
				{Key: "title", Label: "Heading", Type: "text", Max: 40},
				{Key: "category", Label: "Category to show", Type: "text", Hint: "Leave empty for all categories."},
			},
		},
		{
			Type: SectionBusinessInfo, Label: "Business information",
			Blurb: "Address, phone and description from Store information.",
		},
		{
			Type: SectionOpeningHours, Label: "Opening hours",
			Blurb: "Your weekly schedule with a live open/closed badge.",
		},
		{
			Type: SectionFooter, Label: "Footer",
			Blurb: "Store details and the customer sign-in link.",
			Fields: []SectionField{
				{Key: "note", Label: "Footer note", Type: "text", Max: 120},
			},
		},
	}
}

// Section is one entry in the homepage layout.
type Section struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Enabled bool              `json:"enabled"`
	Content map[string]string `json:"content"`
}

// Homepage is the ordered section list.
type Homepage struct {
	Sections []Section `json:"sections"`
}

// Content reads a content key, falling back to a default.
func (s Section) ContentOr(key, fallback string) string {
	if v, ok := s.Content[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

// Visible reports whether the section should render.
func (s Section) Visible() bool { return s.Enabled }

// sectionDefaults is the shipped layout. New tenants start from this.
func defaultSections() []Section {
	return []Section{
		{ID: "announcement", Type: SectionAnnouncement, Enabled: false, Content: map[string]string{}},
		{ID: "hero", Type: SectionHero, Enabled: true, Content: map[string]string{}},
		{ID: "categories", Type: SectionCategories, Enabled: true, Content: map[string]string{"title": "Browse by category"}},
		{ID: "popular", Type: SectionPopular, Enabled: true, Content: map[string]string{"title": "Popular right now"}},
		{ID: "featured", Type: SectionFeatured, Enabled: true, Content: map[string]string{"title": "Chef's picks"}},
		{ID: "menu", Type: SectionMenu, Enabled: true, Content: map[string]string{"title": "Full menu"}},
		{ID: "business", Type: SectionBusinessInfo, Enabled: true, Content: map[string]string{}},
		{ID: "hours", Type: SectionOpeningHours, Enabled: true, Content: map[string]string{}},
		{ID: "footer", Type: SectionFooter, Enabled: true, Content: map[string]string{}},
	}
}

// DefaultHomepage is the starter layout for a tenant that has never edited one.
func DefaultHomepage() Homepage { return Homepage{Sections: defaultSections()} }

var knownSectionTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range SectionTypes() {
		m[t.Type] = true
	}
	return m
}()

// parseHomepage reads a stored homepage document, normalising anything
// unrecognised away. A corrupt document degrades to the default layout rather
// than breaking the storefront.
func parseHomepage(raw []byte) Homepage {
	if len(raw) == 0 {
		return DefaultHomepage()
	}
	var doc struct {
		Sections []Section `json:"sections"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return DefaultHomepage()
	}
	if len(doc.Sections) == 0 {
		return DefaultHomepage()
	}
	seen := map[string]bool{}
	out := make([]Section, 0, len(doc.Sections))
	for i, s := range doc.Sections {
		if !knownSectionTypes[s.Type] {
			continue // unknown type: the storefront could not render it safely
		}
		if s.ID == "" {
			s.ID = s.Type
		}
		if seen[s.ID] {
			s.ID = s.ID + "-" + itoa(i)
		}
		seen[s.ID] = true
		if s.Content == nil {
			s.Content = map[string]string{}
		}
		clean := make(map[string]string, len(s.Content))
		for k, v := range s.Content {
			if len(v) > 2000 {
				v = v[:2000]
			}
			clean[k] = v
		}
		s.Content = clean
		out = append(out, s)
	}
	if len(out) == 0 {
		return DefaultHomepage()
	}
	return Homepage{Sections: out}
}

// EnabledSections returns only the sections that should render, in order.
func (h Homepage) EnabledSections() []Section {
	out := make([]Section, 0, len(h.Sections))
	for _, s := range h.Sections {
		if s.Visible() {
			out = append(out, s)
		}
	}
	return out
}

// Find returns a section by type.
func (h Homepage) Find(t string) (Section, bool) {
	for _, s := range h.Sections {
		if s.Type == t {
			return s, true
		}
	}
	return Section{}, false
}

// NormalizeHomepage validates an incoming homepage document from the admin
// builder: unknown section types are dropped, each section is clamped to its
// declared field set, and the result is guaranteed renderable.
func NormalizeHomepage(in Homepage) Homepage {
	out := Homepage{Sections: make([]Section, 0, len(in.Sections))}
	defs := map[string]SectionTypeDef{}
	for _, d := range SectionTypes() {
		defs[d.Type] = d
	}
	seen := map[string]bool{}
	for i, s := range in.Sections {
		def, ok := defs[s.Type]
		if !ok {
			continue
		}
		if s.ID == "" {
			s.ID = strings.ToLower(s.Type)
		}
		if seen[s.ID] {
			s.ID = s.ID + "-" + itoa(i)
		}
		seen[s.ID] = true
		clean := map[string]string{}
		for _, f := range def.Fields {
			v, ok := s.Content[f.Key]
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if f.Max > 0 && len(v) > f.Max {
				v = v[:f.Max]
			}
			// A field must be one the storefront knows how to render.
			switch f.Type {
			case "image":
				if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
					continue
				}
			case "number":
				v = clampIntString(v, 1, 40)
			case "text", "textarea":
				v = strings.Map(func(r rune) rune {
					if r == '\n' && f.Type == "text" {
						return ' '
					}
					return r
				}, v)
			}
			clean[f.Key] = v
		}
		if s.Type == SectionAnnouncement && clean["text"] == "" {
			// An announcement with no message has nothing to show.
			s.Enabled = false
		}
		if s.Type == SectionHero && len(clean) == 0 {
			s.Enabled = false
		}
		out.Sections = append(out.Sections, Section{
			ID: s.ID, Type: s.Type, Enabled: s.Enabled, Content: clean,
		})
	}
	if len(out.Sections) == 0 {
		return DefaultHomepage()
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func clampIntString(v string, lo, hi int) string {
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return ""
		}
		n = n*10 + int(r-'0')
		if n > hi {
			return ""
		}
	}
	if n < lo {
		n = lo
	}
	return itoa(n)
}
