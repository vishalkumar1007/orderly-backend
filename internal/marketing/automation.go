package marketing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Automation triggers. Each is a question the engine asks the database on a
// schedule rather than an event hook in every business module: orders,
// bookings and consent already leave the rows the questions need.
const (
	TriggerWelcome       = "WELCOME"
	TriggerOrderFollowUp = "ORDER_FOLLOW_UP"
	TriggerWinBack       = "WIN_BACK"
)

var Triggers = map[string]string{
	TriggerWelcome:       "Welcome after consent",
	TriggerOrderFollowUp: "Follow up after an order",
	TriggerWinBack:       "Win back inactive customers",
}

// RuleInput is the editable part of an automation rule.
type RuleInput struct {
	Name         string `json:"name"`
	Trigger      string `json:"trigger"`
	Enabled      *bool  `json:"enabled"`
	Channel      string `json:"channel"`
	DelayHours   int32  `json:"delay_hours"`
	InactiveDays int32  `json:"inactive_days"`
	CooldownDays int32  `json:"cooldown_days"`
	QuietStart   int32  `json:"quiet_start"`
	QuietEnd     int32  `json:"quiet_end"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	ButtonLabel  string `json:"button_label"`
	LinkID       string `json:"link_id"`
	PromotionID  string `json:"promotion_id"`
	TemplateName string `json:"template_name"`
	TemplateLang string `json:"template_lang"`
}

// Clean normalises and validates a rule.
func (in *RuleInput) Clean() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Trigger = strings.ToUpper(strings.TrimSpace(in.Trigger))
	in.Channel = strings.ToUpper(strings.TrimSpace(in.Channel))
	in.Subject = strings.TrimSpace(in.Subject)
	in.Body = strings.TrimSpace(in.Body)
	in.ButtonLabel = strings.TrimSpace(in.ButtonLabel)
	in.LinkID = strings.TrimSpace(in.LinkID)
	in.PromotionID = strings.TrimSpace(in.PromotionID)
	in.TemplateName = strings.TrimSpace(in.TemplateName)
	in.TemplateLang = strings.TrimSpace(in.TemplateLang)
	if in.TemplateLang == "" {
		in.TemplateLang = "en"
	}
	if in.InactiveDays == 0 {
		in.InactiveDays = 45
	}
	switch {
	case in.Name == "" || utf8.RuneCountInString(in.Name) > maxNameLen:
		return errors.New("give the automation a name")
	case Triggers[in.Trigger] == "":
		return errors.New("unknown trigger")
	case !validChannel(in.Channel):
		return errors.New("channel must be EMAIL, SMS or WHATSAPP")
	case in.Body == "":
		return errors.New("write the message")
	case in.Channel == ChannelEmail && in.Subject == "":
		return errors.New("an email needs a subject line")
	case in.Channel == ChannelWhatsApp && in.TemplateName == "":
		return errors.New("WhatsApp needs the name of an approved template")
	case in.DelayHours < 0 || in.DelayHours > 720:
		return errors.New("the delay must be between 0 and 720 hours")
	case in.InactiveDays < 1 || in.InactiveDays > 365:
		return errors.New("inactivity must be between 1 and 365 days")
	case in.CooldownDays < 0 || in.CooldownDays > 365:
		return errors.New("the cooldown must be between 0 and 365 days")
	case in.QuietStart < 0 || in.QuietStart > 23 || in.QuietEnd < 0 || in.QuietEnd > 23:
		return errors.New("quiet hours are 0 to 23")
	case utf8.RuneCountInString(in.Body) > maxBodyLen || utf8.RuneCountInString(in.Subject) > maxSubjectLen:
		return errors.New("the message is too long")
	}
	for _, id := range []string{in.LinkID, in.PromotionID} {
		if id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return errors.New("unknown link or offer")
			}
		}
	}
	return nil
}

// inQuietHours reports whether the business-local hour falls in [start, end),
// a window that may wrap midnight (21 to 9). start == end disables it.
func inQuietHours(start, end int32, now time.Time, tz string) bool {
	if start == end {
		return false
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		loc = time.UTC
	}
	h := int32(now.In(loc).Hour())
	if start < end {
		return h >= start && h < end
	}
	return h >= start || h < end
}

// ruleCampaign is the campaign shape a rule run creates, so preflight and
// Compose treat it exactly like an owner-made campaign.
func ruleCampaign(r sqlc.AutomationRule) sqlc.MarketingCampaign {
	return sqlc.MarketingCampaign{
		TenantID: r.TenantID, Channel: r.Channel, Subject: r.Subject, Body: r.Body, ButtonLabel: r.ButtonLabel,
		TemplateName: r.TemplateName, TemplateLang: r.TemplateLang, PromotionID: r.PromotionID, LinkID: r.LinkID,
	}
}

// runAutomations evaluates every enabled rule once. Each rule that has
// eligible customers becomes one AUTOMATION campaign the normal delivery loop
// sends; automation_runs makes a (rule, customer, trigger) fire exactly once.
func (w *Worker) runAutomations(ctx context.Context) {
	rules, err := w.q.ListEnabledAutomationRules(ctx)
	if err != nil {
		w.log.Error("marketing automation: list rules", "error", err)
		return
	}
	now := time.Now()
	for _, row := range rules {
		if ctx.Err() != nil {
			return
		}
		rule := sqlc.AutomationRule{
			ID: row.ID, TenantID: row.TenantID, Name: row.Name, Trigger: row.Trigger, Enabled: row.Enabled, Channel: row.Channel,
			DelayHours: row.DelayHours, InactiveDays: row.InactiveDays, CooldownDays: row.CooldownDays, QuietStart: row.QuietStart,
			QuietEnd: row.QuietEnd, Subject: row.Subject, Body: row.Body, ButtonLabel: row.ButtonLabel, LinkID: row.LinkID,
			PromotionID: row.PromotionID, TemplateName: row.TemplateName, TemplateLang: row.TemplateLang,
		}
		if inQuietHours(rule.QuietStart, rule.QuietEnd, now, row.TenantTimezone) {
			continue
		}
		w.runRule(ctx, rule)
	}
}

func (w *Worker) runRule(ctx context.Context, rule sqlc.AutomationRule) {
	_ = w.q.TouchAutomationRule(ctx, rule.ID)
	// A rule whose channel cannot send right now waits; nothing is marked
	// as fired, so the customer is still eligible when the provider is back.
	if problem := w.preflight(ctx, ruleCampaign(rule)); problem != "" {
		w.log.Warn("marketing automation: rule not runnable", "rule", pgutil.UUIDString(rule.ID), "reason", problem)
		return
	}
	candidates, err := w.q.AutomationCandidates(ctx, sqlc.AutomationCandidatesParams{
		RuleID: rule.ID, CooldownDays: rule.CooldownDays, TenantID: rule.TenantID, Channel: rule.Channel,
		Trigger: rule.Trigger, DelayHours: rule.DelayHours, InactiveDays: rule.InactiveDays,
	})
	if err != nil {
		w.log.Error("marketing automation: candidates", "rule", pgutil.UUIDString(rule.ID), "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	c, err := w.q.CreateAutomationCampaign(ctx, sqlc.CreateAutomationCampaignParams{
		TenantID: rule.TenantID, Name: fmt.Sprintf("%s: %s", Triggers[rule.Trigger], time.Now().UTC().Format("2 Jan 15:04")),
		Channel: rule.Channel, Subject: rule.Subject, Body: rule.Body, ButtonLabel: rule.ButtonLabel, LinkID: rule.LinkID,
		TemplateName: rule.TemplateName, TemplateLang: rule.TemplateLang, PromotionID: rule.PromotionID,
		AutomationRuleID: rule.ID, TotalCount: 0,
	})
	if err != nil {
		w.log.Error("marketing automation: create campaign", "error", err)
		return
	}
	var total int32
	for _, cand := range candidates {
		// The run row first: if it already exists another process got here,
		// and no recipient is added.
		n, err := w.q.InsertAutomationRun(ctx, sqlc.InsertAutomationRunParams{
			RuleID: rule.ID, TenantID: rule.TenantID, CustomerID: cand.CustomerID, TriggerKey: cand.TriggerKey, CampaignID: c.ID,
		})
		if err != nil || n == 0 {
			continue
		}
		added, err := w.q.InsertAutomationRecipient(ctx, sqlc.InsertAutomationRecipientParams{
			CampaignID: c.ID, Channel: rule.Channel, TenantID: rule.TenantID, CustomerID: cand.CustomerID,
		})
		if err == nil {
			total += int32(added)
		}
	}
	if err := w.q.SetCampaignTotal(ctx, sqlc.SetCampaignTotalParams{ID: c.ID, TotalCount: total}); err != nil {
		w.log.Error("marketing automation: set total", "error", err)
	}
	if total == 0 {
		_ = w.q.FinishCampaign(ctx, sqlc.FinishCampaignParams{ID: c.ID, Status: StatusCompleted, StatusReason: "Nobody new to message."})
	}
}

// RuleTemplates are the starting points the console offers, worded for the
// business type. A template is a suggestion the owner edits, not a rule.
func RuleTemplates(businessType string) []map[string]any {
	visit, thing := "visit", "order"
	switch strings.ToUpper(businessType) {
	case "BARBER", "SALON", "SPA", "AUTOMOTIVE", "SERVICES":
		visit, thing = "appointment", "booking"
	case "HOTEL", "HOSPITALITY":
		visit, thing = "stay", "reservation"
	}
	return []map[string]any{
		{
			"trigger": TriggerWelcome, "name": "Welcome",
			"subject": "Welcome to {{business_name}}",
			"body":    "Thanks for joining us. Here is a little something for your next " + visit + ": {{offer_code}}. {{link}}",
			"hint":    "Sent once, within a week of a customer agreeing to hear from you.",
		},
		{
			"trigger": TriggerOrderFollowUp, "name": "Thank you follow-up", "delay_hours": 24,
			"subject": "How was everything?",
			"body":    "Thank you for your " + thing + " with {{business_name}}. We would love to see you again. {{link}}",
			"hint":    "A day after each completed " + thing + ".",
		},
		{
			"trigger": TriggerWinBack, "name": "We miss you", "inactive_days": 45,
			"subject": "It has been a while",
			"body":    "We have not seen you at {{business_name}} lately. Come back this week and use {{offer_code}}. {{link}}",
			"hint":    "After 45 days without a " + visit + ", at most once a month.",
		},
	}
}

func ruleView(r sqlc.AutomationRule, runs, runs30 int64) map[string]any {
	return map[string]any{
		"id": pgutil.UUIDString(r.ID), "name": r.Name, "trigger": r.Trigger, "trigger_label": Triggers[r.Trigger],
		"enabled": r.Enabled, "channel": r.Channel, "delay_hours": r.DelayHours, "inactive_days": r.InactiveDays,
		"cooldown_days": r.CooldownDays, "quiet_start": r.QuietStart, "quiet_end": r.QuietEnd,
		"subject": r.Subject, "body": r.Body, "button_label": r.ButtonLabel, "link_id": pgutil.UUIDPtr(r.LinkID),
		"promotion_id": pgutil.UUIDPtr(r.PromotionID), "template_name": r.TemplateName, "template_lang": r.TemplateLang,
		"last_run_at": ts(r.LastRunAt), "runs": runs, "runs_30d": runs30,
		"created_at": ts(r.CreatedAt), "updated_at": ts(r.UpdatedAt),
	}
}

func nullUUIDFrom(s string) pgtype.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgutil.UUID(id)
}
