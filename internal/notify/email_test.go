package notify

import (
	"strings"
	"testing"
)

func TestBuildEmailEscapesAndOnlyLinksWebURLs(t *testing.T) {
	text, html := BuildEmail(EmailContent{
		Subject: "Hi", Body: "Hello <script>alert(1)</script>\n\nSecond line",
		ButtonLabel: "Open", ButtonURL: "javascript:alert(1)",
	}, EmailBranding{SenderName: "Momo <Magic>", BrandColor: "red", LogoURL: "data:image/png;base64,xx", ShowLogo: true})

	if strings.Contains(html, "<script>") || strings.Contains(html, "javascript:") {
		t.Fatalf("unsafe content reached the HTML:\n%s", html)
	}
	if strings.Contains(html, "data:image") {
		t.Fatal("a non-web logo URL must be dropped; mail clients block it")
	}
	if _, withButton := BuildEmail(EmailContent{Subject: "S", Body: "B", ButtonLabel: "Go", ButtonURL: "https://x.test"},
		EmailBranding{BrandColor: "red"}); !strings.Contains(withButton, "#0F766E") {
		t.Fatal("an invalid brand colour must fall back to the default")
	}
	if !strings.Contains(html, "Momo &lt;Magic&gt;") {
		t.Fatal("sender name must be escaped and shown when there is no logo")
	}
	if strings.Contains(text, "Open:") {
		t.Fatal("a button without a web URL must not appear in the text part")
	}
}

func TestBuildEmailButtonAndContrast(t *testing.T) {
	text, html := BuildEmail(EmailContent{Subject: "S", Body: "B", ButtonLabel: "Set up", ButtonURL: "https://x.test/a?b=1"},
		EmailBranding{SenderName: "S", BrandColor: "#FDE68A"})
	if !strings.Contains(html, `href="https://x.test/a?b=1"`) {
		t.Fatalf("button link missing:\n%s", html)
	}
	if !strings.Contains(html, "color:#111827") {
		t.Fatal("a light brand colour needs dark button text")
	}
	if !strings.Contains(text, "Set up: https://x.test/a?b=1") {
		t.Fatal("the text part must carry the button link")
	}
}

func TestCustomHTMLEscapesVariablesAndDerivesText(t *testing.T) {
	c := fill(EmailContent{Subject: "Hi {{customer_name}}", HTMLBody: `<p>Hello <b>{{customer_name}}</b></p><a href="https://x.test">Track</a>`},
		[]string{"customer_name"}, map[string]any{"customer_name": "<i>Asha</i>"}, EmailContent{})
	text, html := BuildEmail(c, EmailBranding{})
	if strings.Contains(html, "<i>Asha</i>") || !strings.Contains(html, "&lt;i&gt;Asha&lt;/i&gt;") {
		t.Fatalf("variables in hand-written HTML must be escaped:\n%s", html)
	}
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Fatal("an HTML fragment must be wrapped in a document")
	}
	if !strings.Contains(text, "Hello <i>Asha</i>") || !strings.Contains(text, "Track (https://x.test)") {
		t.Fatalf("derived text part is wrong: %q", text)
	}
}

func TestUnfilledHTMLFallsBackToLayout(t *testing.T) {
	c := fill(EmailContent{Subject: "S", Body: "B", HTMLBody: "<p>{{missing}}</p>"},
		[]string{"missing"}, map[string]any{}, EmailContent{Subject: "S", Body: "B"})
	if c.HTMLBody != "" {
		t.Fatal("HTML with an unfilled variable must fall back to the guided layout")
	}
}

func TestThemesAndRawLayout(t *testing.T) {
	_, dark := BuildEmail(EmailContent{Subject: "S", Body: "B"}, EmailBranding{Theme: "dark", SenderName: "M"})
	if !strings.Contains(dark, "#0E1116") || !strings.Contains(dark, `content="dark"`) {
		t.Fatal("dark theme not applied")
	}
	_, raw := buildEmail(EmailContent{Subject: "S", Body: "Hi {{customer_name}}", ButtonLabel: "Go", ButtonURL: "{{setup_url}}"},
		EmailBranding{SenderName: "M"}, true)
	if !strings.Contains(raw, "{{customer_name}}") || !strings.Contains(raw, `href="{{setup_url}}"`) {
		t.Fatalf("raw layout must keep variables, including in the button link:\n%s", raw)
	}
}

func TestHighlightCodeAndFootnote(t *testing.T) {
	text, html := BuildEmail(EmailContent{Subject: "S", Heading: "Your code", Body: "Use this code.", Highlight: "482913", Footnote: "Don't share it."},
		EmailBranding{SenderName: "Momo", BrandColor: "#0F766E"})
	if !strings.Contains(html, "letter-spacing:7px") || !strings.Contains(html, ">482913<") {
		t.Fatalf("a code highlight must be set as a large monospace code:\n%s", html)
	}
	if !strings.Contains(html, "Don&#39;t share it.") {
		t.Fatal("footnote missing")
	}
	if !strings.Contains(text, "482913") || !strings.Contains(text, "Don't share it.") {
		t.Fatalf("text part must carry the highlight and footnote: %q", text)
	}
	_, order := BuildEmail(EmailContent{Subject: "S", Body: "B", Highlight: "Order #1042"}, EmailBranding{SenderName: "M"})
	if strings.Contains(order, "letter-spacing:7px") {
		t.Fatal("a phrase is not a code and must not be set as one")
	}
}

func TestHTMLFragmentSitsInsideBrandedLayout(t *testing.T) {
	b := EmailBranding{SenderName: "Momo Magic", FooterText: "12 MG Road", Theme: "dark"}
	text, html := BuildEmail(EmailContent{Subject: "S", HTMLBody: `<p>Custom <b>content</b></p>`}, b)
	for _, want := range []string{"<p>Custom <b>content</b></p>", "Momo Magic", "12 MG Road", "#0E1116"} {
		if !strings.Contains(html, want) {
			t.Fatalf("fragment must be wrapped in the themed layout; missing %q", want)
		}
	}
	if !strings.Contains(text, "Custom content") || !strings.Contains(text, "12 MG Road") {
		t.Fatalf("text part: %q", text)
	}

	full := `<!DOCTYPE html><html><body><p>Mine</p></body></html>`
	if _, got := BuildEmail(EmailContent{Subject: "S", HTMLBody: full}, b); got != full {
		t.Fatal("a whole hand-written email must be sent exactly as written")
	}
}

func TestRawContentIsNeutralAndKeepsTokens(t *testing.T) {
	got := RawContent(EmailContent{Heading: "Hi {{customer_name}}", Body: "B", ButtonLabel: "Go", ButtonURL: "{{setup_url}}"},
		EmailBranding{Theme: "dark"})
	if !strings.Contains(got, "{{customer_name}}") || !strings.Contains(got, `href="{{setup_url}}"`) {
		t.Fatalf("starter must keep variables:\n%s", got)
	}
	if !strings.Contains(got, "color:inherit") || strings.Contains(got, "<html") {
		t.Fatalf("starter must be a theme-neutral fragment:\n%s", got)
	}
}

func TestPreheaderDefaultsToStartOfMessage(t *testing.T) {
	if got := preheaderOf(EmailContent{Body: "Your order is ready.\n\nMore."}); got != "Your order is ready." {
		t.Fatalf("got %q", got)
	}
	if got := preheaderOf(EmailContent{Preheader: "Set", Body: "B"}); got != "Set" {
		t.Fatalf("an explicit preview line must win, got %q", got)
	}
}

func TestCustomThemeKeepsTextReadable(t *testing.T) {
	_, dark := BuildEmail(EmailContent{Subject: "S", Body: "B"},
		EmailBranding{SenderName: "M", Theme: "custom", PageColor: "#101418", CardColor: "#1B2430"})
	if !strings.Contains(dark, "background-color:#101418") || !strings.Contains(dark, "#F3F4F6") || !strings.Contains(dark, `content="dark"`) {
		t.Fatal("a dark custom card needs light text")
	}
	_, light := BuildEmail(EmailContent{Subject: "S", Body: "B"},
		EmailBranding{SenderName: "M", Theme: "custom", PageColor: "#FFF7ED", CardColor: "#FFFFFF"})
	if !strings.Contains(light, "#171923") || !strings.Contains(light, "background-color:#FFF7ED") {
		t.Fatal("a light custom card needs dark text")
	}
	_, bad := BuildEmail(EmailContent{Subject: "S", Body: "B"}, EmailBranding{Theme: "custom", PageColor: "red"})
	if !strings.Contains(bad, "#F5F6F8") {
		t.Fatal("invalid custom colours must fall back to the light look")
	}
}

func TestLayoutsShapeTheEmailNotItsColours(t *testing.T) {
	c := EmailContent{Subject: "S", Body: "B", ButtonLabel: "Go", ButtonURL: "https://x.test"}
	b := EmailBranding{SenderName: "Momo", BrandColor: "#0F766E", FooterText: "12 MG Road"}
	render := func(layout, theme string) string {
		b.Layout, b.Theme = layout, theme
		_, h := BuildEmail(c, b)
		return h
	}
	if h := render("header", "classic"); !strings.Contains(h, "background-color:#0F766E;border-radius:10px 10px 0 0") {
		t.Fatal("brand header layout needs the brand band")
	}
	if h := render("centered", "classic"); !strings.Contains(h, "text-align:center") || !strings.Contains(h, `align="center"`) {
		t.Fatal("centered layout must centre text and the button")
	}
	if h := render("plain", "classic"); strings.Contains(h, "border-radius:10px;") {
		t.Fatal("plain layout must not draw a card")
	}
	foot := render("footerbar", "dark")
	if strings.Count(foot, "12 MG Road") != 1 || !strings.Contains(foot, "#0E1116") {
		t.Fatal("footer bar keeps one footer, inside the card, in any theme")
	}
}
