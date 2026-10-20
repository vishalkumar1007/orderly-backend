package marketing

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Handler serves the console's Marketing area and the two public endpoints
// (trackable-link redirect, unsubscribe). Tenant routes are mounted behind
// the storefront permission and the MARKETING plan feature.
type Handler struct {
	q    *sqlc.Queries
	cfg  *configsvc.Service
	urls URLs
	log  *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, cfg *configsvc.Service, urls URLs, log *slog.Logger) *Handler {
	return &Handler{q: sqlc.New(pool), cfg: cfg, urls: urls, log: log}
}

func tenantOf(r *http.Request) (uuid.UUID, string, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return uuid.Nil, "", false
	}
	name := user.Name
	if name == "" {
		name = user.Email
	}
	return *user.TenantID, name, true
}

func idParam(r *http.Request, name string) (pgtype.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return pgtype.UUID{}, false
	}
	return pgutil.UUID(id), true
}

func decode(r *http.Request, into any) bool {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(into) == nil
}

func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error("marketing: "+what, "error", err)
	response.Error(w, http.StatusInternalServerError, "internal_error", "could not "+what)
}

/* ---------- overview ---------- */

// Overview is the landing page in one round trip: numbers, consent posture,
// channel readiness and the latest campaigns.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	tid := pgutil.UUID(tenantID)
	ctx := r.Context()
	stats, err := h.q.MarketingOverview(ctx, tid)
	if err != nil {
		h.internal(w, "load marketing overview", err)
		return
	}
	consent, err := h.q.ConsentSummary(ctx, tid)
	if err != nil {
		h.internal(w, "load consent summary", err)
		return
	}
	campaigns, err := h.q.ListCampaigns(ctx, sqlc.ListCampaignsParams{TenantID: tid, Kind: "", LimitCount: 5})
	if err != nil {
		h.internal(w, "load campaigns", err)
		return
	}
	recent := make([]map[string]any, 0, len(campaigns))
	for _, c := range campaigns {
		recent = append(recent, view(c))
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"stats":     stats,
		"consent":   consent,
		"channels":  h.channels(r, tenantID),
		"recent":    recent,
		"goals":     Goals,
		"audiences": Audiences,
	})
}

// channels reports, per channel, whether a campaign (or a post) could go out
// right now and if not, why: plan or provider. Observed state, never a promise.
func (h *Handler) channels(r *http.Request, tenantID uuid.UUID) map[string]any {
	t, _ := tenantctx.FromContext(r.Context())
	rd := readiness(r.Context(), h.cfg, tenantID)
	entry := func(ready error, allowed bool, notAllowed, notReady string) map[string]any {
		out := map[string]any{"allowed": allowed, "ready": allowed && ready == nil}
		switch {
		case !allowed:
			out["reason"] = notAllowed
		case ready != nil:
			out["reason"] = notReady
		}
		return out
	}
	meta := "Meta is not connected. Set it up under Configuration."
	return map[string]any{
		ChannelEmail:      entry(rd.Email, true, "", "Email is not connected. Set up SMTP under Configuration."),
		ChannelSMS:        entry(rd.SMS, t.HasEntitlement(entitlement.FeatureNotifySMS), "SMS is not included in your plan.", "SMS is not connected. Set up an SMS provider under Configuration."),
		ChannelWhatsApp:   entry(rd.WhatsApp, true, "", "WhatsApp is not connected. Add a Meta phone number id under Configuration."),
		PlatformFacebook:  entry(rd.Facebook, true, "", meta),
		PlatformInstagram: entry(rd.Instagram, true, "", meta),
	}
}

/* ---------- audience and consent ---------- */

func (h *Handler) PreviewAudience(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req struct {
		Audience string `json:"audience"`
		Channel  string `json:"channel"`
	}
	if !decode(r, &req) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	d := Draft{Name: "x", Audience: req.Audience, Channel: req.Channel}
	if err := d.Clean(); err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	row, err := h.q.PreviewAudience(r.Context(), sqlc.PreviewAudienceParams{Channel: d.Channel, TenantID: pgutil.UUID(tenantID), Audience: d.Audience})
	if err != nil {
		h.internal(w, "preview audience", err)
		return
	}
	response.JSON(w, http.StatusOK, row)
}

func (h *Handler) ConsentSummary(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	row, err := h.q.ConsentSummary(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		h.internal(w, "load consent summary", err)
		return
	}
	response.JSON(w, http.StatusOK, row)
}

// GrantAll records owner-attested consent for every customer with an address
// on the channel. The attestation flag is deliberate friction: the owner is
// stating these people agreed to hear from the business.
func (h *Handler) GrantAll(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req struct {
		Channel  string `json:"channel"`
		Attested bool   `json:"attested"`
	}
	if !decode(r, &req) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	ch := strings.ToUpper(strings.TrimSpace(req.Channel))
	if !validChannel(ch) {
		response.Error(w, http.StatusBadRequest, "bad_request", "channel must be EMAIL, SMS or WHATSAPP")
		return
	}
	if !req.Attested {
		response.Error(w, http.StatusBadRequest, "attestation_required", "Confirm that these customers agreed to receive marketing messages.")
		return
	}
	n, err := h.q.GrantConsentForAll(r.Context(), sqlc.GrantConsentForAllParams{Channel: ch, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		h.internal(w, "record consent", err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"granted": n})
}

// SetConsent records one customer's consent or revocation.
func (h *Handler) SetConsent(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req struct {
		CustomerID string `json:"customer_id"`
		Channel    string `json:"channel"`
		Status     string `json:"status"`
	}
	if !decode(r, &req) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	cid, err := uuid.Parse(req.CustomerID)
	ch := strings.ToUpper(strings.TrimSpace(req.Channel))
	st := strings.ToUpper(strings.TrimSpace(req.Status))
	if err != nil || !validChannel(ch) || (st != "GRANTED" && st != "REVOKED") {
		response.Error(w, http.StatusBadRequest, "bad_request", "customer_id, channel (EMAIL|SMS|WHATSAPP) and status (GRANTED|REVOKED) are required")
		return
	}
	// The customer must belong to this tenant: the upsert is scoped, but a
	// foreign id would otherwise create a consent row under our tenant.
	if _, err := h.q.GetCustomerByID(r.Context(), sqlc.GetCustomerByIDParams{ID: pgutil.UUID(cid), TenantID: pgutil.UUID(tenantID)}); err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	if err := h.q.UpsertConsent(r.Context(), sqlc.UpsertConsentParams{
		TenantID: pgutil.UUID(tenantID), CustomerID: pgutil.UUID(cid), Channel: ch, Status: st, Source: "OWNER",
	}); err != nil {
		h.internal(w, "record consent", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- links ---------- */

func (h *Handler) ListLinks(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	t, _ := tenantctx.FromContext(r.Context())
	rows, err := h.q.ListLinks(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		h.internal(w, "load links", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, l := range rows {
		out = append(out, linkView(sqlc.MarketingLink{ID: l.ID, TenantID: l.TenantID, Code: l.Code, Label: l.Label, TargetPath: l.TargetPath, CreatedAt: l.CreatedAt}, l.Visits, t.Slug, h.urls))
	}
	response.JSON(w, http.StatusOK, map[string]any{"links": out})
}

func (h *Handler) CreateLink(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req struct {
		Label      string `json:"label"`
		TargetPath string `json:"target_path"`
	}
	if !decode(r, &req) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	path, err := cleanTargetPath(req.TargetPath)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > maxLabelLen {
		response.Error(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("the label must be %d characters or fewer", maxLabelLen))
		return
	}
	code, err := newCode()
	if err != nil {
		h.internal(w, "create link", err)
		return
	}
	l, err := h.q.CreateLink(r.Context(), sqlc.CreateLinkParams{TenantID: pgutil.UUID(tenantID), Code: code, Label: label, TargetPath: path})
	if err != nil {
		h.internal(w, "create link", err)
		return
	}
	t, _ := tenantctx.FromContext(r.Context())
	response.JSON(w, http.StatusCreated, linkView(l, 0, t.Slug, h.urls))
}

func (h *Handler) DeleteLink(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "link not found")
		return
	}
	n, err := h.q.DeleteLink(r.Context(), sqlc.DeleteLinkParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		h.internal(w, "delete link", err)
		return
	}
	if n == 0 {
		response.Error(w, http.StatusNotFound, "not_found", "link not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// LinkQR renders a link's QR code in the tenant's brand colours, same
// renderer as the storefront QR so a poster and a table card match.
func (h *Handler) LinkQR(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "link not found")
		return
	}
	l, err := h.q.GetLink(r.Context(), sqlc.GetLinkParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "link not found")
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	qr, err := storefront.BuildQRCode(h.urls.Go(l.Code), storefront.QRCodeOptions{
		Size: size, Dark: r.URL.Query().Get("dark"), Light: r.URL.Query().Get("light"),
	})
	if err != nil {
		h.internal(w, "render QR code", err)
		return
	}
	response.JSON(w, http.StatusOK, qr)
}

/* ---------- campaigns ---------- */

func (h *Handler) ListCampaigns(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	kind := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind != "CAMPAIGN" && kind != "AUTOMATION" {
		kind = ""
	}
	rows, err := h.q.ListCampaigns(r.Context(), sqlc.ListCampaignsParams{TenantID: pgutil.UUID(tenantID), Kind: kind, LimitCount: int32(limit)})
	if err != nil {
		h.internal(w, "load campaigns", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, view(c))
	}
	response.JSON(w, http.StatusOK, map[string]any{"campaigns": out})
}

// linkFor validates a draft's link id belongs to the tenant.
func (h *Handler) linkFor(r *http.Request, tenantID uuid.UUID, linkID string) (pgtype.UUID, error) {
	if linkID == "" {
		return pgtype.UUID{}, nil
	}
	id, _ := uuid.Parse(linkID)
	l, err := h.q.GetLink(r.Context(), sqlc.GetLinkParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		return pgtype.UUID{}, errors.New("unknown link")
	}
	return l.ID, nil
}

// promotionFor validates a draft's offer id belongs to the tenant.
func (h *Handler) promotionFor(r *http.Request, tenantID uuid.UUID, promotionID string) (pgtype.UUID, error) {
	if promotionID == "" {
		return pgtype.UUID{}, nil
	}
	id, _ := uuid.Parse(promotionID)
	p, err := h.q.GetPromotion(r.Context(), sqlc.GetPromotionParams{ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		return pgtype.UUID{}, errors.New("unknown offer")
	}
	return p.ID, nil
}

func (h *Handler) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	tenantID, actor, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var d Draft
	if !decode(r, &d) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := d.Clean(); err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	linkID, err := h.linkFor(r, tenantID, d.LinkID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	promoID, err := h.promotionFor(r, tenantID, d.PromotionID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	c, err := h.q.CreateCampaign(r.Context(), sqlc.CreateCampaignParams{
		TenantID: pgutil.UUID(tenantID), Name: d.Name, Goal: d.Goal, Audience: d.Audience, Channel: d.Channel,
		Subject: d.Subject, Body: d.Body, ButtonLabel: d.ButtonLabel, LinkID: linkID, CreatedBy: actor,
		TemplateName: d.TemplateName, TemplateLang: d.TemplateLang, PromotionID: promoID,
	})
	if err != nil {
		h.internal(w, "create campaign", err)
		return
	}
	response.JSON(w, http.StatusCreated, view(c))
}

func (h *Handler) UpdateCampaign(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	var d Draft
	if !decode(r, &d) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := d.Clean(); err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	linkID, err := h.linkFor(r, tenantID, d.LinkID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	promoID, err := h.promotionFor(r, tenantID, d.PromotionID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	c, err := h.q.UpdateCampaignDraft(r.Context(), sqlc.UpdateCampaignDraftParams{
		Name: d.Name, Goal: d.Goal, Audience: d.Audience, Channel: d.Channel, Subject: d.Subject, Body: d.Body,
		ButtonLabel: d.ButtonLabel, LinkID: linkID, TemplateName: d.TemplateName, TemplateLang: d.TemplateLang,
		PromotionID: promoID, ID: id, TenantID: pgutil.UUID(tenantID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Either not ours or no longer a draft. Tell the owner which.
		if _, getErr := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: id, TenantID: pgutil.UUID(tenantID)}); getErr == nil {
			response.Error(w, http.StatusConflict, "not_editable", "Only a draft can be edited. Cancel the campaign and duplicate it to change it.")
			return
		}
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	if err != nil {
		h.internal(w, "update campaign", err)
		return
	}
	response.JSON(w, http.StatusOK, view(c))
}

func (h *Handler) DeleteCampaign(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	n, err := h.q.DeleteDraftCampaign(r.Context(), sqlc.DeleteDraftCampaignParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		h.internal(w, "delete campaign", err)
		return
	}
	if n == 0 {
		response.Error(w, http.StatusConflict, "not_editable", "Only a draft can be deleted.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) GetCampaign(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	tid := pgutil.UUID(tenantID)
	c, err := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: id, TenantID: tid})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	recipients, err := h.q.ListCampaignRecipients(r.Context(), sqlc.ListCampaignRecipientsParams{CampaignID: id, TenantID: tid, LimitCount: 200})
	if err != nil {
		h.internal(w, "load recipients", err)
		return
	}
	visits, _ := h.q.CampaignVisits(r.Context(), sqlc.CampaignVisitsParams{CampaignID: id, TenantID: tid})
	out := view(c)
	out["recipients"] = recipients
	out["visits"] = visits
	if attr, err := h.q.CampaignAttribution(r.Context(), sqlc.CampaignAttributionParams{CampaignID: id, TenantID: tid}); err == nil {
		out["attributed_orders"] = attr.Orders
		out["attributed_revenue"] = pgutil.NumericToFloat(attr.Revenue)
	}
	if c.PromotionID.Valid {
		if p, err := h.q.GetPromotion(r.Context(), sqlc.GetPromotionParams{ID: c.PromotionID, TenantID: tid}); err == nil {
			out["offer_code"] = p.Code
		}
	}
	if c.LinkID.Valid {
		if l, err := h.q.GetLink(r.Context(), sqlc.GetLinkParams{ID: c.LinkID, TenantID: tid}); err == nil {
			t, _ := tenantctx.FromContext(r.Context())
			out["link"] = linkView(l, 0, t.Slug, h.urls)
		}
	}
	response.JSON(w, http.StatusOK, out)
}

// validation is what the review step shows: audience counts with exclusion
// reasons, and every blocker. Shared by Validate and Schedule so the owner
// is never told "ready" and then refused.
func (h *Handler) validation(r *http.Request, tenantID uuid.UUID, c sqlc.MarketingCampaign) (map[string]any, []string, error) {
	tid := pgutil.UUID(tenantID)
	t, _ := tenantctx.FromContext(r.Context())
	problems := Problems(c, t.HasEntitlement, readiness(r.Context(), h.cfg, tenantID))
	audience, err := h.q.PreviewAudience(r.Context(), sqlc.PreviewAudienceParams{Channel: c.Channel, TenantID: tid, Audience: c.Audience})
	if err != nil {
		return nil, nil, err
	}
	if audience.Eligible == 0 {
		problems = append(problems, "Nobody is eligible: no customer in this audience has both an address for this channel and marketing consent.")
	}
	return map[string]any{"audience": audience, "problems": problems, "ready": len(problems) == 0}, problems, nil
}

func (h *Handler) Validate(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	c, err := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	out, _, err := h.validation(r, tenantID, c)
	if err != nil {
		h.internal(w, "validate campaign", err)
		return
	}
	response.JSON(w, http.StatusOK, out)
}

// Schedule moves a draft (or a cancelled-then-fixed draft) to SCHEDULED. No
// scheduled_at means "send now": the worker picks it up on its next tick.
func (h *Handler) Schedule(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	var req struct {
		ScheduledAt string `json:"scheduled_at"`
	}
	_ = decode(r, &req)
	when := time.Now()
	sendNow := true
	if s := strings.TrimSpace(req.ScheduledAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "bad_request", "scheduled_at must be an RFC 3339 time")
			return
		}
		if t.After(time.Now().Add(366 * 24 * time.Hour)) {
			response.Error(w, http.StatusBadRequest, "bad_request", "schedule within the next year")
			return
		}
		if t.After(when) {
			when = t
			sendNow = false
		}
	}
	tid := pgutil.UUID(tenantID)
	c, err := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: id, TenantID: tid})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	_, problems, err := h.validation(r, tenantID, c)
	if err != nil {
		h.internal(w, "validate campaign", err)
		return
	}
	if len(problems) > 0 {
		response.ErrorWithDetails(w, http.StatusUnprocessableEntity, "campaign_not_ready", problems[0], map[string]any{"problems": problems})
		return
	}
	updated, err := h.q.SetCampaignStatus(r.Context(), sqlc.SetCampaignStatusParams{
		Status: StatusScheduled, StatusReason: "", SendNow: sendNow, ScheduledAt: pgutil.Timestamptz(when),
		ID: id, TenantID: tid, FromStatuses: []string{StatusDraft},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusConflict, "invalid_transition", "Only a draft can be scheduled.")
		return
	}
	if err != nil {
		h.internal(w, "schedule campaign", err)
		return
	}
	response.JSON(w, http.StatusOK, view(updated))
}

// Transition handles pause, resume and cancel. Each names the states it may
// leave from; the guarded UPDATE makes two concurrent clicks safe.
func (h *Handler) Transition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, _, ok := tenantOf(r)
		id, okID := idParam(r, "id")
		if !ok || !okID {
			response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
			return
		}
		var to string
		var from []string
		switch action {
		case "pause":
			to, from = StatusPaused, []string{StatusScheduled, StatusQueued, StatusRunning}
		case "resume":
			// Back to SCHEDULED, not RUNNING: a paused campaign that never
			// started has no recipient snapshot yet. The worker tells them
			// apart (QUEUED materialises, RUNNING just continues).
			to, from = StatusScheduled, []string{StatusPaused}
		case "cancel":
			to, from = StatusCancelled, []string{StatusDraft, StatusScheduled, StatusQueued, StatusRunning, StatusPaused}
		case "unschedule":
			to, from = StatusDraft, []string{StatusScheduled}
		default:
			response.Error(w, http.StatusNotFound, "not_found", "unknown action")
			return
		}
		tid := pgutil.UUID(tenantID)
		c, err := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: id, TenantID: tid})
		if err != nil {
			response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
			return
		}
		// A paused campaign that had already started resumes to RUNNING so
		// its remaining recipients are claimed, not re-materialised.
		if action == "resume" && c.StartedAt.Valid {
			to = StatusRunning
		}
		updated, err := h.q.SetCampaignStatus(r.Context(), sqlc.SetCampaignStatusParams{
			Status: to, StatusReason: "", ID: id, TenantID: tid, FromStatuses: from,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			response.Error(w, http.StatusConflict, "invalid_transition", fmt.Sprintf("Cannot %s a campaign that is %s.", action, strings.ToLower(c.Status)))
			return
		}
		if err != nil {
			h.internal(w, action+" campaign", err)
			return
		}
		if action == "cancel" {
			if err := h.q.SkipPendingRecipients(r.Context(), id); err != nil {
				h.log.Error("marketing: skip pending after cancel", "campaign", pgutil.UUIDString(id), "error", err)
			}
		}
		response.JSON(w, http.StatusOK, view(updated))
	}
}

// TestSend delivers the campaign to one address the owner types, so they see
// exactly what customers will get. It bypasses consent (it is the owner's
// own address) and does not touch the campaign's counters.
func (h *Handler) TestSend(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if !decode(r, &req) || strings.TrimSpace(req.To) == "" {
		response.Error(w, http.StatusBadRequest, "bad_request", "an address is required")
		return
	}
	tid := pgutil.UUID(tenantID)
	c, err := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: id, TenantID: tid})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "campaign not found")
		return
	}
	var link *sqlc.MarketingLink
	if c.LinkID.Valid {
		if l, err := h.q.GetLink(r.Context(), sqlc.GetLinkParams{ID: c.LinkID, TenantID: tid}); err == nil {
			link = &l
		}
	}
	offerCode := ""
	if c.PromotionID.Valid {
		if p, err := h.q.GetPromotion(r.Context(), sqlc.GetPromotionParams{ID: c.PromotionID, TenantID: tid}); err == nil {
			offerCode = p.Code
		}
	}
	msg := Compose(r.Context(), h.q, contentOf(c), link, offerCode, "test", h.urls)
	if _, err := Send(r.Context(), h.cfg, tenantID, c.Channel, strings.TrimSpace(req.To), msg); err != nil {
		response.Error(w, http.StatusBadGateway, "send_failed", "The test message could not be sent: "+configsvc.SafeErrorMessage(err))
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"sent_to": strings.TrimSpace(req.To)})
}

/* ---------- public ---------- */

// Go is the trackable-link redirect: /api/v1/go/{code}. No tenant host and no
// session; the code is unguessable and the row names its tenant. The
// destination is always the tenant's own storefront.
func (h *Handler) Go(w http.ResponseWriter, r *http.Request) {
	code := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "code")))
	if len(code) < 6 || len(code) > 32 {
		http.NotFound(w, r)
		return
	}
	l, err := h.q.GetLinkByCode(r.Context(), code)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A campaign may name itself on the link (?c=); it only counts when it
	// belongs to the link's tenant.
	var campaignID pgtype.UUID
	if cid, err := uuid.Parse(r.URL.Query().Get("c")); err == nil {
		if c, err := h.q.GetCampaign(r.Context(), sqlc.GetCampaignParams{ID: pgutil.UUID(cid), TenantID: l.TenantID}); err == nil {
			campaignID = c.ID
		}
	}
	// Best-effort: a visit that fails to record must still redirect.
	if err := h.q.InsertEvent(r.Context(), sqlc.InsertEventParams{TenantID: l.TenantID, Kind: "VISIT", LinkID: l.ID, CampaignID: campaignID}); err != nil {
		h.log.Warn("marketing: record visit", "error", err)
	}
	// The storefront keeps these for the session and sends them back with
	// the order, which is how a sale is attributed to a link or campaign.
	target := h.urls.Target(l.TenantSlug, l.TargetPath)
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	target += sep + "mc=" + code
	if campaignID.Valid {
		target += "&mcid=" + pgutil.UUIDString(campaignID)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

// Unsubscribe revokes the channel consent behind a recipient id and shows a
// plain confirmation page. GET, because mail clients and SMS open links, not
// forms; the id is a one-per-message random UUID, so it cannot be enumerated.
func (h *Handler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, unsubscribePage("This link is not valid.", ""))
		return
	}
	rec, err := h.q.GetRecipientForUnsubscribe(r.Context(), pgutil.UUID(id))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, unsubscribePage("This link is not valid or has expired.", ""))
		return
	}
	if err := h.q.UpsertConsent(r.Context(), sqlc.UpsertConsentParams{
		TenantID: rec.TenantID, CustomerID: rec.CustomerID, Channel: rec.Channel, Status: "REVOKED", Source: "UNSUBSCRIBE",
	}); err != nil {
		h.internal(w, "record unsubscribe", err)
		return
	}
	_ = h.q.InsertEvent(r.Context(), sqlc.InsertEventParams{TenantID: rec.TenantID, Kind: "UNSUBSCRIBE", CampaignID: rec.CampaignID})
	name := "this business"
	if t, err := h.q.GetTenantByID(r.Context(), rec.TenantID); err == nil && t.Name != "" {
		name = t.Name
	}
	fmt.Fprint(w, unsubscribePage("You have been unsubscribed.", "You will no longer receive marketing messages from "+html.EscapeString(name)+" on this channel. Order and booking updates are not affected."))
}

func unsubscribePage(title, body string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` +
		html.EscapeString(title) + `</title><style>body{font-family:system-ui,sans-serif;background:#f6f7f9;color:#111827;display:grid;place-items:center;min-height:100vh;margin:0}main{background:#fff;border-radius:12px;padding:32px;max-width:420px;box-shadow:0 1px 3px rgba(0,0,0,.08)}h1{font-size:20px;margin:0 0 8px}p{margin:0;color:#4b5563;line-height:1.5}</style></head><body><main><h1>` +
		html.EscapeString(title) + `</h1><p>` + body + `</p></main></body></html>`
}

/* ---------- automations ---------- */

func (h *Handler) ListRules(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	rows, err := h.q.ListAutomationRules(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		h.internal(w, "load automations", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, ruleView(sqlc.AutomationRule{
			ID: row.ID, TenantID: row.TenantID, Name: row.Name, Trigger: row.Trigger, Enabled: row.Enabled, Channel: row.Channel,
			DelayHours: row.DelayHours, InactiveDays: row.InactiveDays, CooldownDays: row.CooldownDays, QuietStart: row.QuietStart,
			QuietEnd: row.QuietEnd, Subject: row.Subject, Body: row.Body, ButtonLabel: row.ButtonLabel, LinkID: row.LinkID,
			PromotionID: row.PromotionID, TemplateName: row.TemplateName, TemplateLang: row.TemplateLang, LastRunAt: row.LastRunAt,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, row.Runs, row.Runs30d))
	}
	t, _ := tenantctx.FromContext(r.Context())
	response.JSON(w, http.StatusOK, map[string]any{
		"rules": out, "templates": RuleTemplates(t.BusinessType), "triggers": Triggers, "channels": h.channels(r, tenantID),
	})
}

func (h *Handler) saveRule(w http.ResponseWriter, r *http.Request, existing *sqlc.AutomationRule) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var in RuleInput
	if !decode(r, &in) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if err := in.Clean(); err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	linkID, err := h.linkFor(r, tenantID, in.LinkID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	promoID, err := h.promotionFor(r, tenantID, in.PromotionID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	enabled := true
	if existing != nil {
		enabled = existing.Enabled
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var rule sqlc.AutomationRule
	if existing == nil {
		rule, err = h.q.CreateAutomationRule(r.Context(), sqlc.CreateAutomationRuleParams{
			TenantID: pgutil.UUID(tenantID), Name: in.Name, Trigger: in.Trigger, Enabled: enabled, Channel: in.Channel,
			DelayHours: in.DelayHours, InactiveDays: in.InactiveDays, CooldownDays: in.CooldownDays, QuietStart: in.QuietStart,
			QuietEnd: in.QuietEnd, Subject: in.Subject, Body: in.Body, ButtonLabel: in.ButtonLabel, LinkID: linkID,
			PromotionID: promoID, TemplateName: in.TemplateName, TemplateLang: in.TemplateLang,
		})
	} else {
		rule, err = h.q.UpdateAutomationRule(r.Context(), sqlc.UpdateAutomationRuleParams{
			Name: in.Name, Trigger: in.Trigger, Enabled: enabled, Channel: in.Channel, DelayHours: in.DelayHours,
			InactiveDays: in.InactiveDays, CooldownDays: in.CooldownDays, QuietStart: in.QuietStart, QuietEnd: in.QuietEnd,
			Subject: in.Subject, Body: in.Body, ButtonLabel: in.ButtonLabel, LinkID: linkID, PromotionID: promoID,
			TemplateName: in.TemplateName, TemplateLang: in.TemplateLang, ID: existing.ID, TenantID: pgutil.UUID(tenantID),
		})
	}
	if err != nil {
		h.internal(w, "save automation", err)
		return
	}
	code := http.StatusOK
	if existing == nil {
		code = http.StatusCreated
	}
	response.JSON(w, code, ruleView(rule, 0, 0))
}

func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) { h.saveRule(w, r, nil) }

func (h *Handler) ruleParam(w http.ResponseWriter, r *http.Request) (sqlc.AutomationRule, bool) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "automation not found")
		return sqlc.AutomationRule{}, false
	}
	rule, err := h.q.GetAutomationRule(r.Context(), sqlc.GetAutomationRuleParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "automation not found")
		return sqlc.AutomationRule{}, false
	}
	return rule, true
}

func (h *Handler) UpdateRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := h.ruleParam(w, r)
	if !ok {
		return
	}
	h.saveRule(w, r, &rule)
}

func (h *Handler) SetRuleEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rule, ok := h.ruleParam(w, r)
		if !ok {
			return
		}
		updated, err := h.q.SetAutomationRuleEnabled(r.Context(), sqlc.SetAutomationRuleEnabledParams{Enabled: enabled, ID: rule.ID, TenantID: rule.TenantID})
		if err != nil {
			h.internal(w, "update automation", err)
			return
		}
		response.JSON(w, http.StatusOK, ruleView(updated, 0, 0))
	}
}

func (h *Handler) DeleteRule(w http.ResponseWriter, r *http.Request) {
	rule, ok := h.ruleParam(w, r)
	if !ok {
		return
	}
	if _, err := h.q.DeleteAutomationRule(r.Context(), sqlc.DeleteAutomationRuleParams{ID: rule.ID, TenantID: rule.TenantID}); err != nil {
		h.internal(w, "delete automation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
