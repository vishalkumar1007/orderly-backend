package marketing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

const (
	campaignBatch = 10
	// recipientBatch is the most messages one tick sends in total;
	// perTenantBatch is the most any one (tenant, channel) gets of it, so a
	// big campaign shares the tick with everyone else's.
	recipientBatch = 100
	perTenantBatch = 25
	maxAttempts    = 3
	// automationEvery spaces rule evaluation; rules reason in hours and days.
	automationEvery = time.Minute
)

// backoffFor spaces retries: 1, 4, 16 minutes.
func backoffFor(attempt int) time.Duration {
	d := time.Minute
	for i := 1; i < attempt; i++ {
		d *= 4
	}
	return d
}

// Worker runs the campaign lifecycle: SCHEDULED -> QUEUED (claim),
// QUEUED -> RUNNING (materialise recipients), RUNNING -> deliver in batches,
// RUNNING -> COMPLETED / PARTIALLY_FAILED / FAILED once nothing is left. It
// also evaluates automation rules and publishes due social posts.
//
// Same shape as the notification worker: one goroutine on a ticker, Postgres
// row locks (FOR UPDATE SKIP LOCKED) as the only coordination, so several API
// processes can run it without a broker and without double-sending.
type Worker struct {
	q    *sqlc.Queries
	cfg  *configsvc.Service
	urls URLs
	log  *slog.Logger

	lastAutomation time.Time
}

func NewWorker(q *sqlc.Queries, cfg *configsvc.Service, urls URLs, log *slog.Logger) *Worker {
	return &Worker{q: q, cfg: cfg, urls: urls, log: log}
}

// Start blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one pass of each stage. Exported so a test can drive it.
func (w *Worker) Tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("marketing worker: panicked", "recover", r)
		}
	}()
	if time.Since(w.lastAutomation) >= automationEvery {
		w.lastAutomation = time.Now()
		w.runAutomations(ctx)
	}
	w.startDue(ctx)
	w.deliver(ctx)
	w.finish(ctx)
	w.publishSocial(ctx)
}

// RunAutomationsNow forces a rule evaluation regardless of the schedule.
// Tests and the console's "run now" use it.
func (w *Worker) RunAutomationsNow(ctx context.Context) {
	w.lastAutomation = time.Now()
	w.runAutomations(ctx)
}

// startDue claims due campaigns and materialises their recipients, after
// re-checking what may have changed since scheduling: the plan, the channel
// feature and the provider connection.
func (w *Worker) startDue(ctx context.Context) {
	claimed, err := w.q.ClaimDueCampaigns(ctx, campaignBatch)
	if err != nil {
		w.log.Error("marketing worker: claim campaigns", "error", err)
		return
	}
	for _, c := range claimed {
		w.materialise(ctx, c)
	}
}

// preflight re-checks plan and provider for a tenant and channel. It is the
// same check Problems runs at schedule time, repeated because time passed.
func (w *Worker) preflight(ctx context.Context, c sqlc.MarketingCampaign) (problem string) {
	plan, err := w.q.GetTenantPlanLimits(ctx, c.TenantID)
	if err != nil {
		return "could not load the plan"
	}
	has := planHas(plan.Entitlements)
	if !has(entitlement.FeatureMarketing) {
		return "Marketing is no longer included in the plan."
	}
	if problems := Problems(c, has, readiness(ctx, w.cfg, uuid.UUID(c.TenantID.Bytes))); len(problems) > 0 {
		return problems[0]
	}
	return ""
}

func (w *Worker) materialise(ctx context.Context, c sqlc.MarketingCampaign) {
	fail := func(reason string) {
		w.log.Warn("marketing worker: campaign failed before start", "campaign", pgutil.UUIDString(c.ID), "reason", reason)
		if err := w.q.FinishCampaign(ctx, sqlc.FinishCampaignParams{ID: c.ID, Status: StatusFailed, StatusReason: reason}); err != nil {
			w.log.Error("marketing worker: finish campaign", "error", err)
		}
	}
	if problem := w.preflight(ctx, c); problem != "" {
		fail(problem)
		return
	}
	n, err := w.q.InsertCampaignRecipients(ctx, sqlc.InsertCampaignRecipientsParams{
		CampaignID: c.ID, Channel: c.Channel, TenantID: c.TenantID, Audience: c.Audience,
	})
	if err != nil {
		fail("could not build the recipient list")
		return
	}
	if err := w.q.StartCampaign(ctx, sqlc.StartCampaignParams{ID: c.ID, TotalCount: int32(n)}); err != nil {
		w.log.Error("marketing worker: start campaign", "error", err)
	}
	if n == 0 {
		if err := w.q.FinishCampaign(ctx, sqlc.FinishCampaignParams{ID: c.ID, Status: StatusCompleted, StatusReason: "Nobody was eligible when the campaign started."}); err != nil {
			w.log.Error("marketing worker: finish campaign", "error", err)
		}
	}
}

// campaignCtx is what every recipient of one campaign shares, resolved once
// per batch.
type campaignCtx struct {
	c         sqlc.MarketingCampaign
	link      *sqlc.MarketingLink
	offerCode string
	ok        bool
}

func (w *Worker) lookup(ctx context.Context, cache map[pgtype.UUID]*campaignCtx, id, tenantID pgtype.UUID) *campaignCtx {
	if cc, ok := cache[id]; ok {
		return cc
	}
	cc := &campaignCtx{}
	if c, err := w.q.GetCampaign(ctx, sqlc.GetCampaignParams{ID: id, TenantID: tenantID}); err == nil {
		cc.c, cc.ok = c, true
		if c.LinkID.Valid {
			if l, err := w.q.GetLink(ctx, sqlc.GetLinkParams{ID: c.LinkID, TenantID: tenantID}); err == nil {
				cc.link = &l
			}
		}
		if c.PromotionID.Valid {
			if p, err := w.q.GetPromotion(ctx, sqlc.GetPromotionParams{ID: c.PromotionID, TenantID: tenantID}); err == nil && p.Active {
				cc.offerCode = p.Code
			}
		}
	}
	cache[id] = cc
	return cc
}

// deliver claims a fair-share batch of due recipients across running
// campaigns and sends each one. Consent is re-read per recipient immediately
// before the send, so an unsubscribe that lands mid-campaign is honoured.
//
// ponytail: fixed per-tenant slice per tick (about 2.5 msg/s per tenant per
// channel per process). Make perTenantBatch a plan attribute when a provider
// starts throttling a specific tenant.
func (w *Worker) deliver(ctx context.Context) {
	groups, err := w.q.ListDueSendGroups(ctx)
	if err != nil {
		w.log.Error("marketing worker: list due groups", "error", err)
		return
	}
	cache := map[pgtype.UUID]*campaignCtx{}
	sent := 0
	for _, g := range groups {
		if sent >= recipientBatch || ctx.Err() != nil {
			return
		}
		limit := perTenantBatch
		if left := recipientBatch - sent; left < limit {
			limit = left
		}
		claimed, err := w.q.ClaimDueRecipients(ctx, sqlc.ClaimDueRecipientsParams{TenantID: g.TenantID, Channel: g.Channel, LimitCount: int32(limit)})
		if err != nil {
			w.log.Error("marketing worker: claim recipients", "error", err)
			continue
		}
		for _, r := range claimed {
			if ctx.Err() != nil {
				return
			}
			w.sendOne(ctx, cache, r)
			sent++
		}
	}
}

func (w *Worker) sendOne(ctx context.Context, cache map[pgtype.UUID]*campaignCtx, r sqlc.MarketingRecipient) {
	cc := w.lookup(ctx, cache, r.CampaignID, r.TenantID)
	if !cc.ok {
		w.settle(ctx, r, RecipientFailed, "campaign missing", "")
		return
	}
	if st, err := w.q.GetConsent(ctx, sqlc.GetConsentParams{TenantID: r.TenantID, CustomerID: r.CustomerID, Channel: cc.c.Channel}); err != nil || st != "GRANTED" {
		w.settle(ctx, r, RecipientSkipped, "consent withdrawn", "")
		return
	}
	msg := Compose(ctx, w.q, contentOf(cc.c), cc.link, cc.offerCode, pgutil.UUIDString(r.ID), w.urls)
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	providerID, err := Send(sendCtx, w.cfg, uuid.UUID(r.TenantID.Bytes), cc.c.Channel, r.Address, msg)
	cancel()
	if err == nil {
		w.settle(ctx, r, RecipientSent, "", providerID)
		return
	}
	attempt := int(r.AttemptCount) + 1
	if attempt >= maxAttempts {
		w.settle(ctx, r, RecipientFailed, configsvc.SafeErrorMessage(err), "")
		return
	}
	if uerr := w.q.MarkRecipientAttempt(ctx, sqlc.MarkRecipientAttemptParams{
		ID: r.ID, Status: RecipientRetrying, AttemptCount: int32(attempt),
		LastError: configsvc.SafeErrorMessage(err), NextAttemptAt: pgutil.Timestamptz(time.Now().Add(backoffFor(attempt))),
	}); uerr != nil {
		w.log.Error("marketing worker: mark retry", "error", uerr)
	}
}

// settle writes a recipient's terminal state and bumps the campaign counter
// that goes with it.
func (w *Worker) settle(ctx context.Context, r sqlc.MarketingRecipient, status, lastErr, providerID string) {
	var err error
	bump := sqlc.BumpCampaignCountersParams{ID: r.CampaignID}
	switch status {
	case RecipientSent:
		err = w.q.MarkRecipientSent(ctx, sqlc.MarkRecipientSentParams{ID: r.ID, ProviderMessageID: providerID})
		bump.Sent = 1
	case RecipientFailed:
		err = w.q.MarkRecipientAttempt(ctx, sqlc.MarkRecipientAttemptParams{
			ID: r.ID, Status: status, AttemptCount: r.AttemptCount + 1, LastError: lastErr, NextAttemptAt: pgutil.Timestamptz(time.Now()),
		})
		bump.Failed = 1
	default:
		err = w.q.MarkRecipientAttempt(ctx, sqlc.MarkRecipientAttemptParams{
			ID: r.ID, Status: status, AttemptCount: r.AttemptCount, LastError: lastErr, NextAttemptAt: pgutil.Timestamptz(time.Now()),
		})
		bump.Skipped = 1
	}
	if err != nil {
		w.log.Error("marketing worker: settle recipient", "recipient", pgutil.UUIDString(r.ID), "error", err)
		return
	}
	if err := w.q.BumpCampaignCounters(ctx, bump); err != nil {
		w.log.Error("marketing worker: bump counters", "error", err)
	}
}

// finish closes running campaigns with no work left.
func (w *Worker) finish(ctx context.Context) {
	done, err := w.q.ListRunningCampaignsWithoutWork(ctx)
	if err != nil {
		w.log.Error("marketing worker: list finished", "error", err)
		return
	}
	for _, c := range done {
		status, reason := outcome(c)
		if err := w.q.FinishCampaign(ctx, sqlc.FinishCampaignParams{ID: c.ID, Status: status, StatusReason: reason}); err != nil {
			w.log.Error("marketing worker: finish campaign", "error", err)
		}
	}
}

// outcome maps the counters of a finished campaign to its final state.
func outcome(c sqlc.MarketingCampaign) (status, reason string) {
	switch {
	case c.FailedCount == 0:
		return StatusCompleted, ""
	case c.SentCount == 0:
		return StatusFailed, fmt.Sprintf("All %d messages failed. Check the provider under Configuration.", c.FailedCount)
	default:
		return StatusPartiallyFailed, fmt.Sprintf("%d of %d messages failed.", c.FailedCount, c.TotalCount)
	}
}
