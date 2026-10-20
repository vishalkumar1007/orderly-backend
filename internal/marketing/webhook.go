package marketing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Meta webhook: /api/v1/webhooks/meta. No tenant host and no session. Meta
// proves itself two ways: the subscription handshake echoes a verify token,
// and every delivery carries an HMAC-SHA256 of the body under the app secret.
//
// Two kinds of event matter here. WhatsApp delivery statuses move a recipient
// to DELIVERED, READ or FAILED by provider message id. An inbound text that
// says STOP revokes the sender's WhatsApp consent, by phone, for the tenant
// that owns the receiving number. Everything else is acknowledged and ignored.

// WebhookVerify answers Meta's GET handshake.
func (h *Handler) WebhookVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token := q.Get("hub.verify_token")
	if token == "" || !h.verifyTokenKnown(r, token) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(q.Get("hub.challenge")))
}

func (h *Handler) verifyTokenKnown(r *http.Request, token string) bool {
	if resolved, err := h.cfg.Resolver().Resolve(r.Context(), configsvc.ScopePlatform, nil, configsvc.ServiceMeta); err == nil {
		if want := resolved.String("verify_token"); want != "" && hmac.Equal([]byte(want), []byte(token)) {
			return true
		}
	}
	tokens, err := h.q.MetaVerifyTokens(r.Context())
	if err != nil {
		return false
	}
	for _, t := range tokens {
		if t != "" && hmac.Equal([]byte(t), []byte(token)) {
			return true
		}
	}
	return false
}

type metaWebhook struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Statuses []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Errors []struct {
						Title string `json:"title"`
					} `json:"errors"`
				} `json:"statuses"`
				Messages []struct {
					From string `json:"from"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// Webhook handles Meta's POST deliveries.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var payload metaWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Route to a tenant by the receiving number, then verify the signature
	// with that tenant's app secret (or the platform's). Unverifiable
	// deliveries are refused: a forged status must not flip a recipient.
	var tenantID pgtype.UUID
	for _, e := range payload.Entry {
		for _, c := range e.Changes {
			if pn := c.Value.Metadata.PhoneNumberID; pn != "" {
				if tid, err := h.q.TenantIDByMetaPhoneNumber(r.Context(), pn); err == nil {
					tenantID = tid
				}
			}
		}
	}
	if !h.signatureValid(r, body, tenantID) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	for _, e := range payload.Entry {
		for _, c := range e.Changes {
			for _, s := range c.Value.Statuses {
				h.applyStatus(r, s.ID, s.Status, firstTitle(s.Errors))
			}
			if tenantID.Valid {
				for _, m := range c.Value.Messages {
					if m.Type == "text" && isStop(m.Text.Body) {
						phone := "+" + strings.TrimPrefix(strings.TrimSpace(m.From), "+")
						if _, err := h.q.RevokeConsentByPhone(r.Context(), sqlc.RevokeConsentByPhoneParams{
							Channel: ChannelWhatsApp, TenantID: tenantID, Phone: phone,
						}); err != nil {
							h.log.Warn("meta webhook: revoke consent", "error", err)
						} else {
							_ = h.q.InsertEvent(r.Context(), sqlc.InsertEventParams{TenantID: tenantID, Kind: "UNSUBSCRIBE"})
						}
					}
				}
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func firstTitle(errs []struct {
	Title string `json:"title"`
}) string {
	if len(errs) == 0 {
		return ""
	}
	return errs[0].Title
}

func isStop(text string) bool {
	t := strings.ToUpper(strings.TrimSpace(text))
	return t == "STOP" || t == "UNSUBSCRIBE" || t == "STOP ALL" || t == "CANCEL"
}

// signatureValid checks X-Hub-Signature-256 against the tenant's app secret,
// falling back to the platform's. No secret anywhere means no trust.
func (h *Handler) signatureValid(r *http.Request, body []byte, tenantID pgtype.UUID) bool {
	sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	if sig == "" {
		return false
	}
	var secrets []string
	if tenantID.Valid {
		tid := uuid.UUID(tenantID.Bytes)
		if resolved, err := h.cfg.Resolver().Resolve(r.Context(), configsvc.ScopeTenant, &tid, configsvc.ServiceMeta); err == nil {
			if s := resolved.Secret("app_secret"); s != "" {
				secrets = append(secrets, s)
			}
		}
	}
	if resolved, err := h.cfg.Resolver().Resolve(r.Context(), configsvc.ScopePlatform, nil, configsvc.ServiceMeta); err == nil {
		if s := resolved.Secret("app_secret"); s != "" {
			secrets = append(secrets, s)
		}
	}
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
			return true
		}
	}
	return false
}

// applyStatus maps a WhatsApp status to the recipient that carries the
// message id. Unknown ids are ignored: transactional WhatsApp, if it ever
// exists, is not this module's business.
func (h *Handler) applyStatus(r *http.Request, messageID, status, errTitle string) {
	if messageID == "" {
		return
	}
	rec, err := h.q.GetRecipientByProviderMessage(r.Context(), messageID)
	if err != nil {
		return
	}
	switch strings.ToLower(status) {
	case "delivered":
		_ = h.q.SetRecipientDeliveryStatus(r.Context(), sqlc.SetRecipientDeliveryStatusParams{Status: RecipientDelivered, ID: rec.ID})
	case "read":
		_ = h.q.SetRecipientDeliveryStatus(r.Context(), sqlc.SetRecipientDeliveryStatusParams{Status: RecipientRead, ID: rec.ID})
	case "failed":
		if rec.Status != RecipientSent && rec.Status != RecipientDelivered {
			return
		}
		if errTitle == "" {
			errTitle = "WhatsApp reported the message as failed"
		}
		if err := h.q.MarkRecipientAttempt(r.Context(), sqlc.MarkRecipientAttemptParams{
			ID: rec.ID, Status: RecipientFailed, AttemptCount: rec.AttemptCount, LastError: errTitle, NextAttemptAt: rec.NextAttemptAt,
		}); err == nil {
			// It was counted as sent when the API accepted it; move the count.
			_ = h.q.BumpCampaignCounters(r.Context(), sqlc.BumpCampaignCountersParams{ID: rec.CampaignID, Sent: -1, Failed: 1})
		}
	}
	_ = pgutil.UUIDString
}
