// Package marketing is the growth engine behind the console's Marketing area:
// campaigns, consent, trackable links, automations, social publishing and the
// worker that delivers them.
//
// One engine, several channels. A campaign picks a built-in audience segment
// and one channel; the worker materialises a recipient snapshot (consent and
// address checked at that moment), then sends through the same configsvc
// providers transactional mail and SMS use, plus Meta for WhatsApp. It never
// touches notification_* tables: a promotion has consent, pause and
// unsubscribe semantics an order confirmation does not, and a provider failure
// here must never surface as an order or booking failure.
//
// Gates, outermost first: tenantctx license, the storefront permission
// (owner), and the MARKETING plan feature. SMS campaigns additionally need
// NOTIFY_SMS, re-checked by the worker at run time, because a plan can be
// downgraded between scheduling and sending.
package marketing

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Channels a campaign can use. Facebook and Instagram are publishing targets
// (social planner), not messaging channels, and live in social.go.
const (
	ChannelEmail    = "EMAIL"
	ChannelSMS      = "SMS"
	ChannelWhatsApp = "WHATSAPP"
)

// Channels is every messaging channel, in display order.
var Channels = []string{ChannelEmail, ChannelSMS, ChannelWhatsApp}

func validChannel(c string) bool { return slices.Contains(Channels, c) }

// Goals are the console's entry points ("Bring customers back"). They steer
// the suggested audience and copy; the engine treats them as a label.
var Goals = map[string]string{
	"ACQUIRE":       "Get new customers",
	"RETAIN":        "Bring customers back",
	"PROMOTE":       "Promote a product or service",
	"FILL_SLOW_DAY": "Fill a slow day",
	"ANNOUNCE":      "Share news",
}

// Audiences are the built-in segments. The rule for each lives once, in SQL
// (PreviewAudience / InsertCampaignRecipients), so the preview count and the
// recipient snapshot can never disagree.
var Audiences = map[string]string{
	"ALL":        "Everyone with consent",
	"NEW":        "New in the last 30 days",
	"REPEAT":     "Visited twice or more",
	"INACTIVE":   "No visit in 60 days",
	"HIGH_VALUE": "Four or more visits",
}

// Campaign states (overall). Delivery state lives per recipient.
const (
	StatusDraft           = "DRAFT"
	StatusScheduled       = "SCHEDULED"
	StatusQueued          = "QUEUED"
	StatusRunning         = "RUNNING"
	StatusPaused          = "PAUSED"
	StatusCompleted       = "COMPLETED"
	StatusPartiallyFailed = "PARTIALLY_FAILED"
	StatusFailed          = "FAILED"
	StatusCancelled       = "CANCELLED"
)

// Recipient delivery states. DELIVERED and READ come from provider receipts
// (WhatsApp) and count as sent.
const (
	RecipientPending   = "PENDING"
	RecipientRetrying  = "RETRYING"
	RecipientSent      = "SENT"
	RecipientDelivered = "DELIVERED"
	RecipientRead      = "READ"
	RecipientFailed    = "FAILED"
	RecipientSkipped   = "SKIPPED"
)

const (
	maxNameLen     = 120
	maxSubjectLen  = 200
	maxBodyLen     = 4000
	maxSMSLen      = 480
	maxWhatsAppLen = 1024
	maxLabelLen    = 60
	maxPathLen     = 200
	maxTemplateLen = 512
)

// Draft is the editable part of a campaign, as the console sends it.
type Draft struct {
	Name        string `json:"name"`
	Goal        string `json:"goal"`
	Audience    string `json:"audience"`
	Channel     string `json:"channel"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	ButtonLabel string `json:"button_label"`
	LinkID      string `json:"link_id"`
	// WhatsApp sends an approved template; Body fills its {{1}} parameter.
	TemplateName string `json:"template_name"`
	TemplateLang string `json:"template_lang"`
	// An offer whose code the message can carry as {{offer_code}}.
	PromotionID string `json:"promotion_id"`
}

// Clean normalises a draft and reports the first problem, in the owner's
// words. A draft may be saved incomplete; Problems (below) decides whether it
// can be scheduled.
func (d *Draft) Clean() error {
	d.Name = strings.TrimSpace(d.Name)
	d.Goal = strings.ToUpper(strings.TrimSpace(d.Goal))
	d.Audience = strings.ToUpper(strings.TrimSpace(d.Audience))
	d.Channel = strings.ToUpper(strings.TrimSpace(d.Channel))
	d.Subject = strings.TrimSpace(d.Subject)
	d.Body = strings.TrimSpace(d.Body)
	d.ButtonLabel = strings.TrimSpace(d.ButtonLabel)
	d.LinkID = strings.TrimSpace(d.LinkID)
	d.TemplateName = strings.TrimSpace(d.TemplateName)
	d.TemplateLang = strings.TrimSpace(d.TemplateLang)
	d.PromotionID = strings.TrimSpace(d.PromotionID)
	if d.Goal == "" {
		d.Goal = "PROMOTE"
	}
	if d.Audience == "" {
		d.Audience = "ALL"
	}
	if d.TemplateLang == "" {
		d.TemplateLang = "en"
	}
	switch {
	case d.Name == "":
		return errors.New("give the campaign a name")
	case utf8.RuneCountInString(d.Name) > maxNameLen:
		return fmt.Errorf("the name must be %d characters or fewer", maxNameLen)
	case Goals[d.Goal] == "":
		return errors.New("unknown goal")
	case Audiences[d.Audience] == "":
		return errors.New("unknown audience")
	case !validChannel(d.Channel):
		return errors.New("channel must be EMAIL, SMS or WHATSAPP")
	case utf8.RuneCountInString(d.Subject) > maxSubjectLen:
		return fmt.Errorf("the subject must be %d characters or fewer", maxSubjectLen)
	case utf8.RuneCountInString(d.Body) > maxBodyLen:
		return fmt.Errorf("the message must be %d characters or fewer", maxBodyLen)
	case utf8.RuneCountInString(d.ButtonLabel) > maxLabelLen:
		return fmt.Errorf("the button label must be %d characters or fewer", maxLabelLen)
	case utf8.RuneCountInString(d.TemplateName) > maxTemplateLen || strings.ContainsAny(d.TemplateName, " /"):
		return errors.New("the template name must match the approved template exactly")
	case len(d.TemplateLang) > 10:
		return errors.New("unknown template language")
	}
	for _, id := range []string{d.LinkID, d.PromotionID} {
		if id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return errors.New("unknown link or offer")
			}
		}
	}
	return nil
}

// Readiness is whether each channel has an effective provider for the tenant.
// nil means ready. It is observed state, read from the resolver without
// opening a connection: that is the owner's explicit "Test" under Configuration.
type Readiness struct {
	Email, SMS, WhatsApp, Facebook, Instagram error
}

func (r Readiness) For(channel string) error {
	switch channel {
	case ChannelEmail:
		return r.Email
	case ChannelSMS:
		return r.SMS
	case ChannelWhatsApp:
		return r.WhatsApp
	}
	return fmt.Errorf("unknown channel")
}

var errNotConfigured = errors.New("not configured")

func readiness(ctx context.Context, cfg *configsvc.Service, tenantID uuid.UUID) Readiness {
	var r Readiness
	_, r.Email = cfg.Resolver().Resolve(ctx, configsvc.ScopeTenant, &tenantID, configsvc.ServiceSMTP)
	_, r.SMS = cfg.Resolver().Resolve(ctx, configsvc.ScopeTenant, &tenantID, configsvc.ServiceSMS)
	r.WhatsApp, r.Facebook, r.Instagram = errNotConfigured, errNotConfigured, errNotConfigured
	if p, err := metaFor(ctx, cfg, tenantID); err == nil {
		if p.CanWhatsApp() {
			r.WhatsApp = nil
		}
		if p.CanFacebook() {
			r.Facebook = nil
		}
		if p.CanInstagram() {
			r.Instagram = nil
		}
	} else {
		r.WhatsApp, r.Facebook, r.Instagram = err, err, err
	}
	return r
}

func metaFor(ctx context.Context, cfg *configsvc.Service, tenantID uuid.UUID) (configsvc.MetaProvider, error) {
	resolved, err := cfg.Resolver().Resolve(ctx, configsvc.ScopeTenant, &tenantID, configsvc.ServiceMeta)
	if err != nil {
		return nil, err
	}
	return cfg.Factory().Meta(resolved)
}

// Problems lists everything that stops a campaign from being scheduled:
// missing content, a message that is too long, a plan feature the tenant
// lacks, or a provider that is not connected. Empty means ready.
func Problems(c sqlc.MarketingCampaign, hasFeature func(code string) bool, r Readiness) []string {
	var out []string
	if c.Body == "" {
		out = append(out, "Write the message before sending.")
	}
	switch c.Channel {
	case ChannelEmail:
		if c.Subject == "" {
			out = append(out, "An email needs a subject line.")
		}
		if r.Email != nil {
			out = append(out, "Email is not connected. Set up SMTP under Configuration.")
		}
	case ChannelSMS:
		if utf8.RuneCountInString(c.Body) > maxSMSLen {
			out = append(out, fmt.Sprintf("An SMS must be %d characters or fewer.", maxSMSLen))
		}
		if !hasFeature(entitlement.FeatureNotifySMS) {
			out = append(out, "SMS is not included in your plan.")
		} else if r.SMS != nil {
			out = append(out, "SMS is not connected. Set up an SMS provider under Configuration.")
		}
	case ChannelWhatsApp:
		if c.TemplateName == "" {
			out = append(out, "WhatsApp needs the name of an approved message template.")
		}
		if utf8.RuneCountInString(c.Body) > maxWhatsAppLen {
			out = append(out, fmt.Sprintf("A WhatsApp message must be %d characters or fewer.", maxWhatsAppLen))
		}
		if r.WhatsApp != nil {
			out = append(out, "WhatsApp is not connected. Set up Meta with a phone number id under Configuration.")
		}
	}
	return out
}

// planHas adapts a plan's entitlement list to the predicate Problems takes;
// the HTTP layer passes tenantctx.Info.HasEntitlement instead.
func planHas(entitlements []string) func(string) bool {
	return func(code string) bool { return slices.Contains(entitlements, code) }
}

// URLs builds the public addresses a message carries. Both come from the
// platform's own routing, never from request input.
type URLs struct {
	// APIOrigin is https://api.{base}: the host of /go and /unsubscribe.
	APIOrigin string
	// StoreURL resolves a tenant slug to its storefront origin.
	StoreURL func(slug string) string
}

func (u URLs) Go(code string) string { return u.APIOrigin + "/api/v1/go/" + code }

// GoFor is a tracked link that also names the campaign, so a visit and any
// order that follows can be attributed to it.
func (u URLs) GoFor(code string, campaignID pgtype.UUID) string {
	if !campaignID.Valid {
		return u.Go(code)
	}
	return u.Go(code) + "?c=" + pgutil.UUIDString(campaignID)
}
func (u URLs) Unsubscribe(rid string) string {
	return u.APIOrigin + "/api/v1/marketing/unsubscribe/" + rid
}
func (u URLs) Target(slug, path string) string {
	return strings.TrimRight(u.StoreURL(slug), "/") + path
}

// newCode is a short, URL-safe, globally unique link code: 10 base32 chars
// (50 bits) is plenty against guessing and short enough to type from a poster.
func newCode() (string, error) {
	var b [7]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:10], nil
}

// cleanTargetPath keeps a link inside the tenant's own storefront: a path
// only, no scheme, host or protocol-relative form.
func cleanTargetPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, " \\\r\n") || len(p) > maxPathLen {
		return "", errors.New("the destination must be a path on your storefront, like /menu")
	}
	return p, nil
}

// Message is a composed, ready-to-send campaign message.
type Message struct {
	Subject string
	Text    string
	HTML    string
	// WhatsApp: the approved template and the values for its body parameters.
	Template string
	Lang     string
	Params   []string
}

// Content is what Compose needs from a campaign or an automation rule: the
// two share one renderer so a welcome message and a campaign look alike.
type Content struct {
	TenantID     pgtype.UUID
	CampaignID   pgtype.UUID
	Channel      string
	Subject      string
	Body         string
	ButtonLabel  string
	TemplateName string
	TemplateLang string
}

func contentOf(c sqlc.MarketingCampaign) Content {
	return Content{
		TenantID: c.TenantID, CampaignID: c.ID, Channel: c.Channel, Subject: c.Subject, Body: c.Body,
		ButtonLabel: c.ButtonLabel, TemplateName: c.TemplateName, TemplateLang: c.TemplateLang,
	}
}

// Compose renders one recipient's message. Email goes through the tenant's
// email branding (notify.BuildEmail) so a promotion looks like the shop's
// other mail; SMS is the body plus the link and the opt-out hint; WhatsApp is
// the approved template with the body as its parameter.
//
// Variables: {{business_name}}, {{link}}, {{offer_code}}. Kept to three on
// purpose; a personalisation schema is a template-service concern.
func Compose(ctx context.Context, q *sqlc.Queries, c Content, link *sqlc.MarketingLink, offerCode, recipientID string, urls URLs) Message {
	tenantID := uuid.UUID(c.TenantID.Bytes)
	branding := notify.ResolveBranding(ctx, q, &tenantID)
	linkURL := ""
	if link != nil {
		linkURL = urls.GoFor(link.Code, c.CampaignID)
	}
	vars := strings.NewReplacer("{{business_name}}", branding.SenderName, "{{link}}", linkURL, "{{offer_code}}", offerCode)
	body := vars.Replace(c.Body)
	unsub := urls.Unsubscribe(recipientID)

	switch c.Channel {
	case ChannelSMS:
		text := body
		if linkURL != "" && !strings.Contains(body, linkURL) {
			text += " " + linkURL
		}
		return Message{Text: text + " Reply STOP or open " + unsub + " to opt out."}
	case ChannelWhatsApp:
		text := body
		if linkURL != "" && !strings.Contains(body, linkURL) {
			text += " " + linkURL
		}
		// Meta forbids newlines and runs of spaces inside template parameters.
		text = strings.Join(strings.Fields(text), " ")
		return Message{Text: text, Template: c.TemplateName, Lang: c.TemplateLang, Params: []string{text}}
	}

	content := notify.EmailContent{
		Subject:  vars.Replace(c.Subject),
		Heading:  vars.Replace(c.Subject),
		Body:     body,
		Footnote: "You are receiving this because you agreed to hear from " + branding.SenderName + ". Unsubscribe: " + unsub,
	}
	if offerCode != "" && !strings.Contains(body, offerCode) {
		content.Highlight = "Your code: " + offerCode
	}
	if linkURL != "" {
		content.ButtonURL = linkURL
		content.ButtonLabel = c.ButtonLabel
		if content.ButtonLabel == "" {
			content.ButtonLabel = "Visit " + branding.SenderName
		}
	}
	text, html := notify.BuildEmail(content, branding)
	return Message{Subject: content.Subject, Text: text, HTML: html}
}

// Send delivers one composed message through the tenant's effective provider.
func Send(ctx context.Context, cfg *configsvc.Service, tenantID uuid.UUID, channel, to string, m Message) (providerID string, err error) {
	switch channel {
	case ChannelEmail:
		return "", cfg.Notifications().Send(ctx, &tenantID, configsvc.MailMessage{
			To: []string{to}, Subject: m.Subject, Body: m.Text, HTMLBody: m.HTML,
		})
	case ChannelSMS:
		return cfg.SMSService().Send(ctx, &tenantID, to, m.Text)
	case ChannelWhatsApp:
		p, err := metaFor(ctx, cfg, tenantID)
		if err != nil {
			return "", err
		}
		return p.SendWhatsAppTemplate(ctx, to, m.Template, m.Lang, m.Params)
	}
	return "", fmt.Errorf("unsupported channel %q", channel)
}

// view is the campaign as the console sees it: plain JSON types, no pgtype.
func view(c sqlc.MarketingCampaign) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(c.ID), "name": c.Name, "goal": c.Goal, "goal_label": Goals[c.Goal],
		"audience": c.Audience, "audience_label": Audiences[c.Audience], "channel": c.Channel, "kind": c.Kind,
		"subject": c.Subject, "body": c.Body, "button_label": c.ButtonLabel, "link_id": pgutil.UUIDPtr(c.LinkID),
		"template_name": c.TemplateName, "template_lang": c.TemplateLang, "promotion_id": pgutil.UUIDPtr(c.PromotionID),
		"automation_rule_id": pgutil.UUIDPtr(c.AutomationRuleID),
		"status":             c.Status, "status_reason": c.StatusReason,
		"scheduled_at": ts(c.ScheduledAt), "started_at": ts(c.StartedAt), "finished_at": ts(c.FinishedAt),
		"total_count": c.TotalCount, "sent_count": c.SentCount, "failed_count": c.FailedCount, "skipped_count": c.SkippedCount,
		"created_by": c.CreatedBy, "created_at": ts(c.CreatedAt), "updated_at": ts(c.UpdatedAt),
	}
}

func ts(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func linkView(l sqlc.MarketingLink, visits int64, slug string, urls URLs) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(l.ID), "code": l.Code, "label": l.Label, "target_path": l.TargetPath,
		"url": urls.Go(l.Code), "target_url": urls.Target(slug, l.TargetPath), "visits": visits,
		"created_at": l.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// Attribution is what a storefront checkout carries back from a tracked
// link: the link code and, when the link was in a campaign, the campaign.
type Attribution struct {
	LinkCode   string `json:"link_code"`
	CampaignID string `json:"campaign_id"`
}

// ResolveAttribution turns the storefront's claim into verified ids: the link
// and campaign must belong to the ordering tenant, or they are dropped. A
// customer cannot credit another shop's campaign with their order.
func ResolveAttribution(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, a Attribution) (linkID, campaignID pgtype.UUID) {
	if code := strings.ToLower(strings.TrimSpace(a.LinkCode)); code != "" {
		if l, err := q.GetLinkByCode(ctx, code); err == nil && uuid.UUID(l.TenantID.Bytes) == tenantID {
			linkID = l.ID
		}
	}
	if id, err := uuid.Parse(strings.TrimSpace(a.CampaignID)); err == nil {
		if c, err := q.GetCampaign(ctx, sqlc.GetCampaignParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID)}); err == nil {
			campaignID = c.ID
		}
	}
	return
}
