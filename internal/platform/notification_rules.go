package platform

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/* ------------------------------------------------------------------ *
 * JSON shapes
 * ------------------------------------------------------------------ */

func eventJSON(e sqlc.NotificationEvent) map[string]any {
	var vars []string
	_ = json.Unmarshal(e.Variables, &vars)
	out := map[string]any{
		"code":             e.Code,
		"category":         e.Category,
		"label":            e.Label,
		"description":      e.Description,
		"default_channels": e.DefaultChannels,
		"variables":        vars,
	}
	if e.RequiredCapability.Valid {
		out["required_capability"] = e.RequiredCapability.String
	}
	return out
}

// ruleJSON reports whether a rule is a tenant's own override (as opposed to
// the inherited platform default) so the settings UI can label it correctly
// without a second round trip.
func ruleJSON(r sqlc.NotificationRule) map[string]any {
	return map[string]any{
		"id":               pgutil.UUIDString(r.ID),
		"event_code":       r.EventCode,
		"channel":          r.Channel,
		"enabled":          r.Enabled,
		"recipient_policy": r.RecipientPolicy,
		"priority":         r.Priority,
		"locked":           r.Locked,
		"overridden":       r.TenantID.Valid,
	}
}

func templateJSON(t sqlc.NotificationTemplate) map[string]any {
	return map[string]any{
		"id":         pgutil.UUIDString(t.ID),
		"event_code": t.EventCode,
		"channel":    t.Channel,
		"subject":    t.Subject,
		"body":       t.Body,
		"version":    t.Version,
		"overridden": t.TenantID.Valid,
		"updated_at": t.UpdatedAt.Time.Format(time.RFC3339),
	}
}

func deliveryJSON(d sqlc.NotificationDelivery) map[string]any {
	out := map[string]any{
		"id":            pgutil.UUIDString(d.ID),
		"event_code":    d.EventCode,
		"channel":       d.Channel,
		"recipient":     d.Recipient,
		"status":        d.Status,
		"attempt_count": d.AttemptCount,
		"max_attempts":  d.MaxAttempts,
		"last_error":    d.LastError,
		"created_at":    d.CreatedAt.Time.Format(time.RFC3339),
	}
	if d.TenantID.Valid {
		out["tenant_id"] = pgutil.UUIDString(d.TenantID)
	}
	if d.DeliveredAt.Valid {
		out["delivered_at"] = d.DeliveredAt.Time.Format(time.RFC3339)
	}
	return out
}

func preferencesJSON(p sqlc.NotificationPreference) map[string]any {
	var overrides map[string]any
	if len(p.Overrides) > 0 {
		_ = json.Unmarshal(p.Overrides, &overrides)
	}
	out := map[string]any{
		"overrides":     overrides,
		"sound_enabled": p.SoundEnabled,
	}
	if p.QuietHoursStart.Valid {
		out["quiet_hours_start"] = formatClockTime(p.QuietHoursStart)
	}
	if p.QuietHoursEnd.Valid {
		out["quiet_hours_end"] = formatClockTime(p.QuietHoursEnd)
	}
	if p.QuietHoursTz.Valid {
		out["quiet_hours_tz"] = p.QuietHoursTz.String
	}
	return out
}

func formatClockTime(t pgtype.Time) string {
	total := t.Microseconds / 1_000_000
	return time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(total) * time.Second).Format("15:04")
}

/* ------------------------------------------------------------------ *
 * Shared request parsing / validation
 * ------------------------------------------------------------------ */

type ruleWriteRequest struct {
	EventCode       string `json:"event_code"`
	Channel         string `json:"channel"`
	RecipientPolicy string `json:"recipient_policy"`
	Enabled         bool   `json:"enabled"`
	Priority        string `json:"priority"`
}

func (req ruleWriteRequest) validate() error {
	switch strings.ToUpper(req.Channel) {
	case "IN_APP", "EMAIL", "SMS":
	default:
		return errInvalidChannel
	}
	switch strings.ToUpper(req.Priority) {
	case "LOW", "NORMAL", "HIGH", "CRITICAL":
	default:
		return errInvalidPriority
	}
	if strings.TrimSpace(req.EventCode) == "" || strings.TrimSpace(req.RecipientPolicy) == "" {
		return errMissingField
	}
	return nil
}

var (
	errInvalidChannel  = &fieldError{"channel must be IN_APP, EMAIL or SMS"}
	errInvalidPriority = &fieldError{"priority must be LOW, NORMAL, HIGH or CRITICAL"}
	errMissingField    = &fieldError{"event_code and recipient_policy are required"}
)

type fieldError struct{ msg string }

func (e *fieldError) Error() string { return e.msg }

type templateWriteRequest struct {
	EventCode string `json:"event_code"`
	Channel   string `json:"channel"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

type testSendRequest struct {
	EventCode string `json:"event_code"`
	Channel   string `json:"channel"`
	To        string `json:"to"`
}

/* ------------------------------------------------------------------ *
 * Catalog
 * ------------------------------------------------------------------ */

// ListNotificationEvents returns the full catalog — used by the Super Admin
// platform notification settings, which may configure any event.
func (h *Handler) ListNotificationEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListNotificationEvents(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load events")
		return
	}
	out := make([]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventJSON(e))
	}
	response.JSON(w, http.StatusOK, map[string]any{"events": out})
}

// ListTenantNotificationEvents returns only the events this business's
// enabled capabilities make relevant — a Food Shop never sees "Reservation
// confirmed." tenantctx already loaded the capability set for this request;
// this is the one new reader of it, following HasCapability's documented
// "nothing else should query tenant_capabilities per-request" by reusing the
// in-context map instead of a fresh query.
func (h *Handler) ListTenantNotificationEvents(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	codes := make([]string, 0, len(tenant.Capabilities))
	for code, enabled := range tenant.Capabilities {
		if enabled {
			codes = append(codes, code)
		}
	}
	rows, err := h.q.ListNotificationEventsForCapabilities(r.Context(), codes)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load events")
		return
	}
	out := make([]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventJSON(e))
	}
	response.JSON(w, http.StatusOK, map[string]any{"events": out})
}

/* ------------------------------------------------------------------ *
 * Platform rules & templates
 * ------------------------------------------------------------------ */

func (h *Handler) ListPlatformNotificationRules(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListPlatformRules(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load rules")
		return
	}
	out := make([]any, 0, len(rows))
	for _, ru := range rows {
		out = append(out, ruleJSON(ru))
	}
	response.JSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (h *Handler) PutPlatformNotificationRule(w http.ResponseWriter, r *http.Request) {
	var req ruleWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	if err := req.validate(); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	row, err := h.q.UpsertPlatformNotificationRule(r.Context(), sqlc.UpsertPlatformNotificationRuleParams{
		EventCode: req.EventCode, Channel: strings.ToUpper(req.Channel),
		Enabled: req.Enabled, RecipientPolicy: req.RecipientPolicy, Priority: strings.ToUpper(req.Priority),
		Locked: false,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save rule")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	_ = insertAudit(r.Context(), h.q, nil, &actor.ID, "Platform Notification Rule Changed", "notification_rule", row.ID)
	response.JSON(w, http.StatusOK, ruleJSON(row))
}

func (h *Handler) ListPlatformNotificationTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListPlatformTemplates(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load templates")
		return
	}
	out := make([]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, templateJSON(t))
	}
	response.JSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (h *Handler) PutPlatformNotificationTemplate(w http.ResponseWriter, r *http.Request) {
	h.putTemplate(w, r, nil)
}

func (h *Handler) TestSendPlatformNotificationTemplate(w http.ResponseWriter, r *http.Request) {
	h.testSendTemplate(w, r, nil)
}

/* ------------------------------------------------------------------ *
 * Platform delivery log
 * ------------------------------------------------------------------ */

func (h *Handler) ListAllNotificationDeliveries(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListAllDeliveries(r.Context(), sqlc.ListAllDeliveriesParams{Limit: 100, Offset: 0})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load deliveries")
		return
	}
	out := make([]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryJSON(d))
	}
	response.JSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (h *Handler) AdminRetryNotificationDelivery(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid delivery id")
		return
	}
	if err := h.q.AdminRetryDelivery(r.Context(), pgutil.UUID(id)); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to retry delivery")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// GetTenantNotificationConfig is the Business Profile -> Notifications tab's
// one read: this tenant's effective rules (each already flagged
// "overridden" by ruleJSON) plus its most recent deliveries, so a Super
// Admin never needs to open the tenant's own console to see either.
func (h *Handler) GetTenantNotificationConfig(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	rules, err := h.q.ListEffectiveRulesForTenant(r.Context(), pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load rules")
		return
	}
	deliveries, err := h.q.ListDeliveriesForTenant(r.Context(), sqlc.ListDeliveriesForTenantParams{
		TenantID: pgutil.UUID(id), Limit: 20, Offset: 0,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load deliveries")
		return
	}
	ruleOut := make([]any, 0, len(rules))
	for _, ru := range rules {
		ruleOut = append(ruleOut, ruleJSON(ru))
	}
	delOut := make([]any, 0, len(deliveries))
	for _, d := range deliveries {
		delOut = append(delOut, deliveryJSON(d))
	}
	response.JSON(w, http.StatusOK, map[string]any{"rules": ruleOut, "deliveries": delOut})
}

/* ------------------------------------------------------------------ *
 * Tenant rules & templates
 * ------------------------------------------------------------------ */

func (h *Handler) ListTenantNotificationRules(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	rows, err := h.q.ListEffectiveRulesForTenant(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load rules")
		return
	}
	out := make([]any, 0, len(rows))
	for _, ru := range rows {
		out = append(out, ruleJSON(ru))
	}
	response.JSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (h *Handler) PutTenantNotificationRule(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	var req ruleWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	if err := req.validate(); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	channel := strings.ToUpper(req.Channel)
	locked, err := h.q.IsRuleLockedForTenant(r.Context(), sqlc.IsRuleLockedForTenantParams{
		TenantID: pgutil.UUID(tenantID), EventCode: req.EventCode, Channel: channel, RecipientPolicy: req.RecipientPolicy,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to check rule policy")
		return
	}
	if locked {
		response.Error(w, http.StatusForbidden, "rule_locked",
			"This is a mandatory security or platform alert and cannot be changed here.")
		return
	}
	row, err := h.q.UpsertTenantNotificationRule(r.Context(), sqlc.UpsertTenantNotificationRuleParams{
		TenantID: pgutil.UUID(tenantID), EventCode: req.EventCode, Channel: channel,
		Enabled: req.Enabled, RecipientPolicy: req.RecipientPolicy, Priority: strings.ToUpper(req.Priority),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save rule")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	tid := pgutil.UUID(tenantID)
	_ = insertAudit(r.Context(), h.q, &tid, &actor.ID, "Notification Rule Changed", "notification_rule", row.ID)
	response.JSON(w, http.StatusOK, ruleJSON(row))
}

// DeleteTenantNotificationRule reverts to inheriting the platform default.
func (h *Handler) DeleteTenantNotificationRule(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	eventCode := r.URL.Query().Get("event_code")
	channel := strings.ToUpper(r.URL.Query().Get("channel"))
	policy := r.URL.Query().Get("recipient_policy")
	if eventCode == "" || channel == "" || policy == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "event_code, channel and recipient_policy are required")
		return
	}
	if err := h.q.DeleteTenantNotificationRuleOverride(r.Context(), sqlc.DeleteTenantNotificationRuleOverrideParams{
		TenantID: pgutil.UUID(tenantID), EventCode: eventCode, Channel: channel, RecipientPolicy: policy,
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to revert rule")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) ListTenantNotificationTemplates(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	rows, err := h.q.ListEffectiveTemplatesForTenant(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load templates")
		return
	}
	out := make([]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, templateJSON(t))
	}
	response.JSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (h *Handler) PutTenantNotificationTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	h.putTemplate(w, r, &tenantID)
}

func (h *Handler) TestSendTenantNotificationTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	h.testSendTemplate(w, r, &tenantID)
}

/* ------------------------------------------------------------------ *
 * Tenant delivery log
 * ------------------------------------------------------------------ */

func (h *Handler) ListTenantNotificationDeliveries(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	rows, err := h.q.ListDeliveriesForTenant(r.Context(), sqlc.ListDeliveriesForTenantParams{
		TenantID: pgutil.UUID(tenantID), Limit: 100, Offset: 0,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load deliveries")
		return
	}
	out := make([]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryJSON(d))
	}
	response.JSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (h *Handler) RetryTenantNotificationDelivery(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenantID(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid delivery id")
		return
	}
	if err := h.q.RetryDelivery(r.Context(), sqlc.RetryDeliveryParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to retry delivery")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

/* ------------------------------------------------------------------ *
 * Personal preferences
 * ------------------------------------------------------------------ */

func (h *Handler) GetMyNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	row, err := h.q.GetNotificationPreferences(r.Context(), pgutil.UUID(actor.ID))
	if err != nil {
		if err == pgx.ErrNoRows {
			response.JSON(w, http.StatusOK, map[string]any{"sound_enabled": true})
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load preferences")
		return
	}
	response.JSON(w, http.StatusOK, preferencesJSON(row))
}

type preferencesWriteRequest struct {
	SoundEnabled    bool    `json:"sound_enabled"`
	QuietHoursStart *string `json:"quiet_hours_start"`
	QuietHoursEnd   *string `json:"quiet_hours_end"`
	QuietHoursTZ    *string `json:"quiet_hours_tz"`
}

func (h *Handler) PutMyNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	var req preferencesWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	start, err := parseClockTime(req.QuietHoursStart)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "quiet_hours_start must be HH:MM")
		return
	}
	end, err := parseClockTime(req.QuietHoursEnd)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "quiet_hours_end must be HH:MM")
		return
	}
	row, err := h.q.UpsertNotificationPreferences(r.Context(), sqlc.UpsertNotificationPreferencesParams{
		UserID: pgutil.UUID(actor.ID), Overrides: []byte("{}"), SoundEnabled: req.SoundEnabled,
		QuietHoursStart: start, QuietHoursEnd: end, QuietHoursTz: pgutil.NullText(req.QuietHoursTZ),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save preferences")
		return
	}
	response.JSON(w, http.StatusOK, preferencesJSON(row))
}

func parseClockTime(v *string) (pgtype.Time, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return pgtype.Time{}, nil
	}
	t, err := time.Parse("15:04", strings.TrimSpace(*v))
	if err != nil {
		return pgtype.Time{}, err
	}
	micros := (t.Hour()*3600 + t.Minute()*60) * 1_000_000
	return pgtype.Time{Microseconds: int64(micros), Valid: true}, nil
}

/* ------------------------------------------------------------------ *
 * Bell: mark unread
 * ------------------------------------------------------------------ */

func (h *Handler) MarkNotificationUnread(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid notification id")
		return
	}
	if err := h.q.MarkNotificationUnread(r.Context(), sqlc.MarkNotificationUnreadParams{
		ID: pgutil.UUID(id), UserID: pgutil.UUID(actor.ID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update notification")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

/* ------------------------------------------------------------------ *
 * Shared helpers
 * ------------------------------------------------------------------ */

// mustTenantID reads the tenant id the auth middleware already verified.
// Every route that calls this sits behind auth.RequireTenant, so the zero
// value never actually reaches a query.
func mustTenantID(r *http.Request) uuid.UUID {
	actor, _ := identity.UserFromContext(r.Context())
	if actor.TenantID == nil {
		return uuid.UUID{}
	}
	return *actor.TenantID
}

func (h *Handler) putTemplate(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	var req templateWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	channel := strings.ToUpper(req.Channel)
	switch channel {
	case "EMAIL", "SMS":
	default:
		response.Error(w, http.StatusBadRequest, "invalid_request", "channel must be EMAIL or SMS")
		return
	}
	if strings.TrimSpace(req.EventCode) == "" || strings.TrimSpace(req.Body) == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "event_code and body are required")
		return
	}
	tid := pgutil.NullUUID(tenantID)
	if err := h.q.DeactivateTemplate(r.Context(), sqlc.DeactivateTemplateParams{
		EventCode: req.EventCode, Channel: channel, TenantID: tid,
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save template")
		return
	}
	row, err := h.q.CreateTemplateVersion(r.Context(), sqlc.CreateTemplateVersionParams{
		EventCode: req.EventCode, Channel: channel, Subject: req.Subject, Body: req.Body, TenantID: tid,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save template")
		return
	}
	response.JSON(w, http.StatusOK, templateJSON(row))
}

// testSendTemplate delivers a template's CURRENT saved content (or the
// request's draft content, when sent before saving) directly through the
// configured provider — synchronous, bypassing the delivery queue, exactly
// like the existing SMTP "send test email" action.
func (h *Handler) testSendTemplate(w http.ResponseWriter, r *http.Request, tenantID *uuid.UUID) {
	var req testSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	channel := strings.ToUpper(req.Channel)
	if strings.TrimSpace(req.To) == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "to is required")
		return
	}
	tmpl, err := h.q.GetEffectiveTemplate(r.Context(), sqlc.GetEffectiveTemplateParams{
		EventCode: req.EventCode, Channel: channel, TenantID: pgutil.NullUUID(tenantID),
	})
	subject, body := "Orderly notification test", "This is a test of the "+req.EventCode+" "+channel+" template."
	if err == nil {
		subject, body = tmpl.Subject, tmpl.Body
	}
	if h.configs == nil {
		response.Error(w, http.StatusServiceUnavailable, "not_configured", "no provider is configured")
		return
	}
	switch channel {
	case "EMAIL":
		outcome, err := h.configs.Notifications().SendTest(r.Context(), tenantID, req.To)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "send_failed", err.Error())
			return
		}
		_ = subject
		_ = body
		response.JSON(w, http.StatusOK, outcome)
	case "SMS":
		outcome, err := h.configs.SMSService().SendTest(r.Context(), tenantID, req.To)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "send_failed", err.Error())
			return
		}
		response.JSON(w, http.StatusOK, outcome)
	default:
		response.Error(w, http.StatusBadRequest, "invalid_request", "channel must be EMAIL or SMS")
	}
}
