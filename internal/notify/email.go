package notify

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// EmailContent is one email's editable copy. Every field may hold {{var}}
// tokens until it is filled. The guided parts follow how a transactional email
// is read: heading, message, one highlighted detail (a code, an order number),
// an optional button, and a quiet note. HTMLBody, when set, is the whole email
// written by hand and replaces the guided layout; Body is then its text version.
type EmailContent struct {
	Subject     string `json:"subject"`
	Preheader   string `json:"preheader"`
	Heading     string `json:"heading"`
	Body        string `json:"body"`
	Highlight   string `json:"highlight"`
	ButtonLabel string `json:"button_label"`
	ButtonURL   string `json:"button_url"`
	Footnote    string `json:"footnote"`
	HTMLBody    string `json:"html_body"`
}

// EmailBranding is the look shared by every email of one sender.
type EmailBranding struct {
	SenderName string `json:"sender_name"`
	ShowLogo   bool   `json:"show_logo"`
	LogoURL    string `json:"logo_url"`
	BrandColor string `json:"brand_color"`
	FooterText string `json:"footer_text"`
	Theme      string `json:"theme"`
	// Layout arranges header, card and footer; see Layouts.
	Layout string `json:"layout"`
	// PageColor and CardColor are used by the "custom" theme only.
	PageColor string `json:"page_color"`
	CardColor string `json:"card_color"`
}

const defaultBrandColor = "#0F766E"

// Themes are the guided layout's looks. Every value is a fixed literal, so
// nothing a user types reaches a style attribute except the validated colour.
type theme struct {
	Scheme                      string // color-scheme meta
	Page, Card, Border          string
	Heading, Body, Muted, Faint string
	Boxed, Band, Dark           bool // card border, brand header band, dark surfaces
}

var themes = map[string]theme{
	"classic": {Scheme: "light", Page: "#F5F6F8", Card: "#FFFFFF", Border: "#E5E7EB",
		Heading: "#171923", Body: "#4B5563", Muted: "#6B7280", Faint: "#9CA3AF", Boxed: true},
	"minimal": {Scheme: "light", Page: "#FFFFFF", Card: "#FFFFFF", Border: "#FFFFFF",
		Heading: "#171923", Body: "#4B5563", Muted: "#6B7280", Faint: "#9CA3AF"},
	"bold": {Scheme: "light", Page: "#F5F6F8", Card: "#FFFFFF", Border: "#E5E7EB",
		Heading: "#171923", Body: "#4B5563", Muted: "#6B7280", Faint: "#9CA3AF", Boxed: true, Band: true},
	"dark": {Scheme: "dark", Page: "#0E1116", Card: "#161B22", Border: "#262C36",
		Heading: "#F3F4F6", Body: "#C9CED6", Muted: "#9AA3AF", Faint: "#6B7280", Boxed: true, Dark: true},
}

// ThemeNames is the allowed theme list, for validation.
// Layouts are how an email is arranged, independent of its colours:
// classic (logo above a card), header (logo on a brand band), centered,
// plain (no card, like a letter) and footerbar (footer inside the card).
func Layouts() []string { return []string{"classic", "header", "centered", "plain", "footerbar"} }

func ThemeNames() []string { return []string{"classic", "minimal", "bold", "dark", "custom"} }

// customTheme builds a look from two chosen colours. Text colours come from
// the card's luminance, so any pick stays readable.
func customTheme(page, card string) theme {
	if !hexColor.MatchString(page) || !hexColor.MatchString(card) {
		return themes["classic"]
	}
	page, card = strings.ToUpper(page), strings.ToUpper(card)
	dark := readableOn(card) == "#FFFFFF"
	ink := "#171923"
	scheme := "light"
	if dark {
		ink, scheme = "#F3F4F6", "dark"
	}
	return theme{
		Scheme: scheme, Page: page, Card: card,
		Border:  mix(ink, card, 0.12),
		Heading: ink,
		Body:    mix(ink, card, 0.80),
		Muted:   mix(ink, card, 0.62),
		Faint:   mix(ink, card, 0.45),
		Boxed:   page != card,
		Dark:    dark,
	}
}

var (
	hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	// A highlight that looks like a code (short, no spaces, has a digit) is set
	// large in monospace with wide tracking, so it can be read and typed.
	codeLike = regexp.MustCompile(`^[A-Za-z0-9-]{4,12}$`)
	hasDigit = regexp.MustCompile(`[0-9]`)
)

// BuildEmail lays content out as an HTML email and its plain-text twin: the
// hand-written HTML when there is one, else the guided layout in the sender's
// theme. The layout is table-based with inline styles, which is what mail
// clients render; html/template escapes every value placed into it.
func BuildEmail(c EmailContent, b EmailBranding) (text, html string) {
	return buildEmail(c, b, false)
}

// isFullDocument tells a hand-written whole email from a content fragment
// meant to sit inside the branded layout.
func isFullDocument(h string) bool {
	h = strings.ToLower(h)
	return strings.Contains(h, "<html") || strings.Contains(h, "<!doctype")
}

// layoutData is what the guided layout renders from. raw keeps {{tokens}} in
// links (for "start from the design"); neutral drops the theme's text colours
// so a generated fragment inherits whichever theme it is later placed in.
func layoutData(c EmailContent, b EmailBranding, raw, neutral bool) (data map[string]any, button, heading string) {
	color := strings.ToUpper(b.BrandColor)
	if !hexColor.MatchString(color) {
		color = defaultBrandColor
	}
	t, ok := themes[b.Theme]
	if b.Theme == "custom" {
		t, ok = customTheme(b.PageColor, b.CardColor), true
	}
	if !ok {
		t = themes["classic"]
	}
	// Structure comes from the layout, never the colours.
	t.Boxed, t.Band = b.Layout != "plain", b.Layout == "header"
	if b.Layout == "plain" {
		t.Page = t.Card
	}
	if neutral {
		t.Heading, t.Body = "inherit", "inherit"
	}
	linkOK := func(u string) bool { return isWebURL(u) || (raw && strings.HasPrefix(strings.TrimSpace(u), "{{")) }
	if c.ButtonLabel != "" && linkOK(c.ButtonURL) {
		button = strings.TrimSpace(c.ButtonURL)
	}
	logo := ""
	if b.ShowLogo && isWebURL(b.LogoURL) {
		logo = b.LogoURL
	}
	heading = c.Heading
	if strings.TrimSpace(heading) == "" {
		heading = c.Subject
	}
	highlight := strings.TrimSpace(c.Highlight)
	isCode := codeLike.MatchString(highlight) && hasDigit.MatchString(highlight)
	if raw && strings.HasPrefix(highlight, "{{") && strings.Contains(strings.ToLower(highlight), "code") {
		isCode = true
	}

	// The highlight box is a tint of the brand colour, never a second accent.
	hlBg, hlBorder, hlInk := mix(color, "#FFFFFF", 0.06), mix(color, "#FFFFFF", 0.22), mix(color, "#000000", 0.25)
	if t.Dark {
		hlBg, hlBorder, hlInk = mix(color, t.Card, 0.14), mix(color, t.Card, 0.35), mix(color, "#FFFFFF", 0.45)
	}
	return map[string]any{
		"T":           t,
		"Preheader":   preheaderOf(c),
		"Heading":     heading,
		"Paragraphs":  paragraphs(c.Body),
		"Highlight":   highlight,
		"IsCode":      isCode,
		"ButtonLabel": c.ButtonLabel,
		"ButtonURL":   template.URL(button),
		"Footnote":    paragraphs(c.Footnote),
		"LogoURL":     template.URL(logo),
		"Sender":      b.SenderName,
		"Footer":      b.FooterText,
		"Year":        time.Now().Year(),
		"Brand":       template.CSS(color),
		"OnBrand":     template.CSS(readableOn(color)),
		"HlBg":        template.CSS(hlBg),
		"HlBorder":    template.CSS(hlBorder),
		"HlInk":       template.CSS(hlInk),
		"Center":      b.Layout == "centered",
		"FootIn":      b.Layout == "footerbar",
		"FootBg":      template.CSS(mix(t.Border, t.Card, 0.45)),
	}, button, heading
}

// preheaderOf is the inbox preview line: the one set, else the start of the
// message, so nobody has to write it for every email.
func preheaderOf(c EmailContent) string {
	if p := strings.TrimSpace(c.Preheader); p != "" {
		return p
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(c.Body), "\n", 2)[0])
	if r := []rune(first); len(r) > 110 {
		return string(r[:107]) + "..."
	}
	return first
}

// keepTokens undoes html/template's percent-encoding of {{ }} inside href.
var keepTokens = strings.NewReplacer("%7b%7b", "{{", "%7d%7d", "}}", "%7B%7B", "{{", "%7D%7D", "}}")

// RawContent is the guided content block as HTML with the {{variables}} left
// in and theme-neutral text colours: the starting point for writing an
// email's content by hand while keeping the branded layout around it.
func RawContent(c EmailContent, b EmailBranding) string {
	data, _, _ := layoutData(c, b, true, true)
	var buf bytes.Buffer
	_ = emailLayout.ExecuteTemplate(&buf, "content", data)
	return strings.TrimSpace(keepTokens.Replace(buf.String()))
}

// buildEmail with raw keeps {{tokens}} in links, for "start from the design".
func buildEmail(c EmailContent, b EmailBranding, raw bool) (text, htmlOut string) {
	custom := strings.TrimSpace(c.HTMLBody)
	if raw {
		custom = ""
	}
	// A whole hand-written email is sent exactly as written.
	if custom != "" && isFullDocument(custom) {
		text = strings.TrimSpace(c.Body)
		if text == "" {
			text = HTMLToText(custom)
		}
		return text + "\n", custom
	}

	data, button, heading := layoutData(c, b, raw, false)
	if custom != "" {
		// A fragment replaces the guided content inside the branded layout.
		data["Custom"] = template.HTML(custom)
	}
	var buf bytes.Buffer
	_ = emailLayout.Execute(&buf, data)
	htmlOut = buf.String()
	if raw {
		htmlOut = keepTokens.Replace(htmlOut)
	}
	if custom != "" {
		text = strings.TrimSpace(c.Body)
		if text == "" {
			text = HTMLToText(custom)
		}
		if b.FooterText != "" {
			text += "\n\n--\n" + b.FooterText
		}
		return text + "\n", htmlOut
	}

	var tb strings.Builder
	tb.WriteString(strings.TrimSpace(heading) + "\n\n" + strings.TrimSpace(c.Body))
	if highlight := strings.TrimSpace(c.Highlight); highlight != "" {
		tb.WriteString("\n\n    " + highlight)
	}
	if button != "" {
		tb.WriteString("\n\n" + c.ButtonLabel + ": " + button)
	}
	if f := strings.TrimSpace(c.Footnote); f != "" {
		tb.WriteString("\n\n" + f)
	}
	if b.FooterText != "" {
		tb.WriteString("\n\n--\n" + b.FooterText)
	}
	return tb.String() + "\n", htmlOut
}

// mix blends hex colour a toward b by weight w of a (w=0.06 is a faint tint).
func mix(a, b string, w float64) string {
	ch := func(hex string, i int) float64 {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		return float64(v)
	}
	out := "#"
	for i := 0; i < 3; i++ {
		out += fmt.Sprintf("%02X", int(ch(a, i)*w+ch(b, i)*(1-w)+0.5))
	}
	return out
}

// paragraphs splits a body on blank lines; single newlines stay line breaks.
func paragraphs(body string) [][]string {
	body = strings.ReplaceAll(strings.TrimSpace(body), "\r\n", "\n")
	var out [][]string
	for _, p := range strings.Split(body, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.Split(p, "\n"))
		}
	}
	return out
}

func isWebURL(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// readableOn picks white or near-black text for a button colour, by luminance.
func readableOn(hex string) string {
	r, _ := strconv.ParseUint(hex[1:3], 16, 8)
	g, _ := strconv.ParseUint(hex[3:5], 16, 8)
	b, _ := strconv.ParseUint(hex[5:7], 16, 8)
	if 0.299*float64(r)+0.587*float64(g)+0.114*float64(b) > 160 {
		return "#111827"
	}
	return "#FFFFFF"
}

var (
	reDropBlock = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`),
		regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`),
	}
	reLink      = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)
	reBreak     = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|tr|h[1-6]|li|table|blockquote)>`)
	reTag       = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlankRuns = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText derives the plain-text alternative of a hand-written HTML email:
// links keep their address, blocks become lines, tags and entities go.
func HTMLToText(doc string) string {
	for _, re := range reDropBlock {
		doc = re.ReplaceAllString(doc, "")
	}
	doc = reLink.ReplaceAllStringFunc(doc, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		label := strings.TrimSpace(reTag.ReplaceAllString(parts[2], ""))
		if label == "" || label == parts[1] {
			return parts[1]
		}
		return label + " (" + parts[1] + ")"
	})
	doc = reBreak.ReplaceAllString(doc, "\n")
	doc = html.UnescapeString(reTag.ReplaceAllString(doc, ""))
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.TrimSpace(reBlankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// The guided layout: brand line, one quiet card, footer. Deliberately plain:
// it should read like mail from a real business, not a marketing template.
var emailLayout = template.Must(template.New("email").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta name="color-scheme" content="{{.T.Scheme}}">
<meta name="supported-color-schemes" content="{{.T.Scheme}}">
<title>{{.Heading}}</title>
</head>
<body style="margin:0;padding:0;background-color:{{.T.Page}};font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:{{.T.Heading}};-webkit-font-smoothing:antialiased;">
<div style="display:none;font-size:1px;color:{{.T.Page}};line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;">{{.Preheader}}&#847;&zwnj;&nbsp;&#847;&zwnj;&nbsp;&#847;&zwnj;&nbsp;</div>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="background-color:{{.T.Page}};">
<tr><td align="center" style="padding:40px 16px;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="max-width:520px;">
{{if .T.Band}}<tr><td style="background-color:{{.Brand}};border-radius:10px 10px 0 0;padding:22px 32px;text-align:{{if .Center}}center{{else}}left{{end}};">{{if .LogoURL}}<img src="{{.LogoURL}}" alt="{{.Sender}}" height="32" style="display:block;height:32px;max-width:180px;width:auto;border:0;{{if .Center}}margin:0 auto;{{end}}">{{else}}<span style="font-size:18px;line-height:26px;font-weight:700;letter-spacing:-0.4px;color:{{.OnBrand}};">{{.Sender}}</span>{{end}}</td></tr>
{{else}}<tr><td style="padding:0 0 20px {{if .Center}}0{{else}}2px{{end}};text-align:{{if .Center}}center{{else}}left{{end}};">{{if .LogoURL}}<img src="{{.LogoURL}}" alt="{{.Sender}}" height="32" style="display:block;height:32px;max-width:180px;width:auto;border:0;{{if .Center}}margin:0 auto;{{end}}">{{else}}<span style="font-size:20px;line-height:28px;font-weight:700;letter-spacing:-0.5px;color:{{.T.Heading}};">{{.Sender}}</span>{{end}}</td></tr>
{{end}}<tr><td style="background-color:{{.T.Card}};{{if .T.Boxed}}border:1px solid {{.T.Border}};{{if .T.Band}}border-top:0;border-radius:0 0 10px 10px;{{else}}border-radius:10px;{{end}}{{else}}border-top:1px solid {{.T.Border}};{{end}}overflow:hidden;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0"><tr><td style="padding:{{if .T.Boxed}}{{if .Center}}36px 32px 12px{{else}}32px 32px 8px{{end}}{{else}}28px 2px 4px{{end}};color:{{.T.Heading}};font-size:15px;line-height:24px;text-align:{{if .Center}}center{{else}}left{{end}};">
{{if .Custom}}{{.Custom}}{{else}}{{template "content" .}}{{end}}</td></tr>
{{if .FootIn}}<tr><td style="background-color:{{.FootBg}};border-top:1px solid {{.T.Border}};padding:16px 32px;">{{if .Footer}}<p style="margin:0 0 4px;font-size:12px;line-height:19px;color:{{.T.Muted}};">{{.Footer}}</p>{{end}}<p style="margin:0;font-size:12px;line-height:19px;color:{{.T.Faint}};">&copy; {{.Year}} {{.Sender}}</p></td></tr>{{end}}</table>
</td></tr>
{{if not .FootIn}}<tr><td style="padding:20px {{if .Center}}0{{else}}2px{{end}} 0;text-align:{{if .Center}}center{{else}}left{{end}};{{if not .T.Boxed}}border-top:1px solid {{.T.Border}};{{end}}">{{if .Footer}}<p style="margin:0 0 6px;font-size:12px;line-height:19px;color:{{.T.Muted}};">{{.Footer}}</p>{{end}}<p style="margin:0;font-size:12px;line-height:19px;color:{{.T.Faint}};">&copy; {{.Year}} {{.Sender}}</p></td></tr>{{end}}
</table>
</td></tr>
</table>
</body>
</html>{{define "content"}}<h1 style="margin:0 0 14px;font-size:23px;line-height:31px;font-weight:650;letter-spacing:-0.4px;color:{{.T.Heading}};">{{.Heading}}</h1>
{{range .Paragraphs}}<p style="margin:0 0 20px;font-size:15px;line-height:24px;color:{{$.T.Body}};">{{range $i, $line := .}}{{if $i}}<br>{{end}}{{$line}}{{end}}</p>
{{end}}{{if .Highlight}}<table role="presentation" {{if .Center}}align="center" {{end}}cellspacing="0" cellpadding="0" border="0" style="margin:4px {{if .Center}}auto{{else}}0{{end}} 24px;"><tr><td align="center" bgcolor="{{.HlBg}}" style="padding:{{if .IsCode}}17px 24px{{else}}12px 18px{{end}};border:1px solid {{.HlBorder}};border-radius:8px;">{{if .IsCode}}<span style="font-family:SFMono-Regular,Consolas,'Liberation Mono',Menlo,monospace;font-size:30px;line-height:38px;font-weight:700;letter-spacing:7px;color:{{.HlInk}};">{{.Highlight}}</span>{{else}}<span style="font-size:17px;line-height:24px;font-weight:650;letter-spacing:-0.2px;color:{{.HlInk}};">{{.Highlight}}</span>{{end}}</td></tr></table>
{{end}}{{if .ButtonURL}}<table role="presentation" {{if .Center}}align="center" {{end}}cellspacing="0" cellpadding="0" border="0" style="margin:4px {{if .Center}}auto{{else}}0{{end}} 24px;"><tr><td bgcolor="{{.Brand}}" style="border-radius:8px;"><a href="{{.ButtonURL}}" target="_blank" style="display:inline-block;padding:12px 22px;font-size:15px;line-height:20px;font-weight:600;color:{{.OnBrand}};text-decoration:none;border-radius:8px;">{{.ButtonLabel}}</a></td></tr></table>
{{end}}{{range .Footnote}}<p style="margin:0 0 20px;font-size:14px;line-height:22px;color:{{$.T.Muted}};">{{range $i, $line := .}}{{if $i}}<br>{{end}}{{$line}}{{end}}</p>
{{end}}{{if .ButtonURL}}<p style="margin:0 0 24px;font-size:12px;line-height:19px;color:{{.T.Faint}};">If the button doesn't work, paste this link into your browser:<br><a href="{{.ButtonURL}}" style="color:{{.T.Muted}};word-break:break-all;">{{.ButtonURL}}</a></p>
{{end}}{{end}}`))

// ResolveBranding is the effective look for a sender: the business's own
// email branding, else its storefront identity on the platform's look; the
// platform's row for platform mail (tenantID nil).
func ResolveBranding(ctx context.Context, q *sqlc.Queries, tenantID *uuid.UUID) EmailBranding {
	out := EmailBranding{SenderName: "Orderly", ShowLogo: true, BrandColor: defaultBrandColor, Theme: "classic"}
	if p, err := q.GetEmailBranding(ctx, pgutil.NullUUID(nil)); err == nil {
		out.ShowLogo, out.LogoURL, out.BrandColor, out.FooterText, out.Theme = p.ShowLogo, p.LogoUrl, p.BrandColor, p.FooterText, p.Theme
		out.PageColor, out.CardColor, out.Layout = p.PageColor, p.CardColor, p.Layout
	}
	if tenantID == nil {
		return out
	}
	out.LogoURL, out.FooterText = "", ""
	if id, err := q.GetTenantEmailIdentity(ctx, pgutil.UUID(*tenantID)); err == nil {
		out.SenderName, out.LogoURL = id.DisplayName, id.LogoUrl
	}
	if t, err := q.GetEmailBranding(ctx, pgutil.NullUUID(tenantID)); err == nil {
		out.ShowLogo, out.BrandColor, out.FooterText, out.Theme = t.ShowLogo, t.BrandColor, t.FooterText, t.Theme
		out.PageColor, out.CardColor, out.Layout = t.PageColor, t.CardColor, t.Layout
		if t.LogoUrl != "" {
			out.LogoURL = t.LogoUrl
		}
	}
	if out.FooterText == "" {
		out.FooterText = "You are receiving this email from " + out.SenderName + "."
	}
	return out
}

func contentOf(t sqlc.NotificationTemplate) EmailContent {
	return EmailContent{
		Subject: t.Subject, Preheader: t.Preheader, Heading: t.Heading, Body: t.Body,
		ButtonLabel: t.ButtonLabel, ButtonURL: t.ButtonUrl, HTMLBody: t.HtmlBody,
		Highlight: t.Highlight, Footnote: t.Footnote,
	}
}

// emailVariables is an event's declared variables plus business_name, which
// every email can use: it is the sender's own name.
func emailVariables(ctx context.Context, q *sqlc.Queries, eventCode string) []string {
	vars := allowedVariables(ctx, q, eventCode)
	for _, v := range vars {
		if v == "business_name" {
			return vars
		}
	}
	return append(vars, "business_name")
}

// fill substitutes variables into every field. Subject and body fall back to
// the sender's own copy when a variable is unfilled; optional fields are
// dropped instead (hand-written HTML falls back to the guided layout), so a raw
// "{{token}}" never reaches a recipient. Values going into HTML are escaped.
func fill(c EmailContent, allowed []string, data map[string]any, fallback EmailContent) EmailContent {
	optional := func(v string, d map[string]any) string { return filled(substitute(v, allowed, d), allowed, "\x00") }
	escaped := make(map[string]any, len(data))
	for k, v := range data {
		escaped[k] = html.EscapeString(fmt.Sprint(v))
	}
	out := EmailContent{
		Subject:     filled(substitute(orFallback(c.Subject, fallback.Subject), allowed, data), allowed, fallback.Subject),
		Body:        filled(substitute(orFallback(c.Body, fallback.Body), allowed, data), allowed, fallback.Body),
		Preheader:   optional(c.Preheader, data),
		Heading:     optional(c.Heading, data),
		ButtonLabel: optional(c.ButtonLabel, data),
		ButtonURL:   optional(c.ButtonURL, data),
		Highlight:   optional(c.Highlight, data),
		Footnote:    optional(c.Footnote, data),
		HTMLBody:    optional(c.HTMLBody, escaped),
	}
	for _, f := range []*string{&out.Preheader, &out.Heading, &out.ButtonLabel, &out.ButtonURL, &out.Highlight, &out.Footnote, &out.HTMLBody} {
		if *f == "\x00" {
			*f = ""
		}
	}
	return out
}

// ComposeEmail renders an event's email for sending: the tenant's template,
// else the platform default, else the caller's fallback copy: in the sender's
// branding, as HTML with a plain-text alternative.
func ComposeEmail(ctx context.Context, q *sqlc.Queries, tenantID *uuid.UUID, eventCode string, data map[string]any, fallback EmailContent, to string) configsvc.MailMessage {
	branding := ResolveBranding(ctx, q, tenantID)
	vars := map[string]any{"business_name": branding.SenderName}
	for k, v := range data {
		vars[k] = v
	}
	content := fallback
	if tmpl, err := q.GetEffectiveTemplate(ctx, sqlc.GetEffectiveTemplateParams{
		EventCode: eventCode, Channel: ChannelEmail, TenantID: pgutil.NullUUID(tenantID),
	}); err == nil {
		content = fill(contentOf(tmpl), emailVariables(ctx, q, eventCode), vars, fallback)
	}
	text, html := BuildEmail(content, branding)
	return configsvc.MailMessage{To: []string{to}, Subject: content.Subject, Body: text, HTMLBody: html}
}

// PreviewEmail renders draft content (saved or not) with realistic sample
// values, in the given branding: what the editor shows and a test sends.
// raw renders the guided layout with the {{tokens}} left in, as a starting
// point for writing the email in HTML.
func PreviewEmail(ctx context.Context, q *sqlc.Queries, eventCode string, draft EmailContent, branding EmailBranding, raw bool) (subject, text, html string) {
	if raw {
		draft.HTMLBody = ""
		text, html = buildEmail(draft, branding, true)
		return draft.Subject, text, html
	}
	allowed := emailVariables(ctx, q, eventCode)
	data := map[string]any{}
	for _, name := range allowed {
		data[name] = sampleValue(name, branding.SenderName)
	}
	c := fill(draft, allowed, data, EmailContent{Subject: draft.Subject, Body: draft.Body})
	text, html = BuildEmail(c, branding)
	return c.Subject, text, html
}

// SavedContent is the effective template's content for an event, or a stub.
func SavedContent(ctx context.Context, q *sqlc.Queries, tenantID *uuid.UUID, eventCode string) EmailContent {
	tmpl, err := q.GetEffectiveTemplate(ctx, sqlc.GetEffectiveTemplateParams{
		EventCode: eventCode, Channel: ChannelEmail, TenantID: pgutil.NullUUID(tenantID),
	})
	if err != nil {
		return EmailContent{Subject: "Test: " + eventCode, Body: "This is a test of the " + eventCode + " email."}
	}
	return contentOf(tmpl)
}

var samples = map[string]string{
	"customer_name": "Asha Rao", "order_number": "1042", "order_id": "ORD-1042",
	"total": "450.00", "amount": "450.00", "failure_reason": "the card was declined",
	"product_name": "Paneer Momos", "product_id": "PRD-17", "quantity": "4", "threshold": "10", "unit": "plates",
	"starts_at": "Sat, 25 Oct · 5:30 PM", "check_in": "25 Oct", "check_out": "27 Oct",
	"appointment_id": "APT-208", "reservation_id": "RES-311",
	"user_name": "Rahul Mehta", "user_email": "rahul@example.com", "user_id": "USR-9", "new_role": "Manager", "method": "Authenticator app",
	"tenant_name": "Momo Magic", "tenant_id": "TEN-1", "plan_name": "Growth",
	"setup_url": "https://example.com/setup-password?token=sample", "code": "482913", "minutes": "10",
	"message": "Payment webhooks are being retried.",
}

func sampleValue(name, sender string) string {
	// A business previews with its own name; the platform previews a business
	// email with a sample one ("Set up Orderly on Orderly" reads as a bug).
	if name == "business_name" && sender != "" && sender != "Orderly" {
		return sender
	}
	if name == "business_name" {
		return "Momo Magic"
	}
	if v, ok := samples[name]; ok {
		return v
	}
	return "[" + strings.ReplaceAll(name, "_", " ") + "]"
}
