package marketing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Social planner: drafts, a schedule, and publishing through the official
// Meta Graph API when the tenant has connected a Page or an Instagram
// professional account. Without a connection a post is MANUAL: the owner gets
// the caption, image and link to share by hand, which is honest about what the
// platform can do rather than a greyed-out button.

const (
	PlatformFacebook  = "FACEBOOK"
	PlatformInstagram = "INSTAGRAM"

	SocialDraft      = "DRAFT"
	SocialScheduled  = "SCHEDULED"
	SocialPublishing = "PUBLISHING"
	SocialPublished  = "PUBLISHED"
	SocialPartial    = "PARTIAL"
	SocialFailed     = "FAILED"
	SocialManual     = "MANUAL"
)

var platforms = []string{PlatformFacebook, PlatformInstagram}

const (
	maxCaptionLen = 2200 // Instagram's limit; Facebook allows more.
	socialBatch   = 10
)

// PostInput is the editable part of a post.
type PostInput struct {
	Caption     string   `json:"caption"`
	ImageURL    string   `json:"image_url"`
	Platforms   []string `json:"platforms"`
	LinkID      string   `json:"link_id"`
	ScheduledAt string   `json:"scheduled_at"`
}

func (in *PostInput) clean() (pgtype.Timestamptz, error) {
	in.Caption = strings.TrimSpace(in.Caption)
	in.ImageURL = strings.TrimSpace(in.ImageURL)
	in.LinkID = strings.TrimSpace(in.LinkID)
	seen := map[string]bool{}
	cleaned := []string{}
	for _, p := range in.Platforms {
		p = strings.ToUpper(strings.TrimSpace(p))
		if !slices.Contains(platforms, p) {
			return pgtype.Timestamptz{}, errors.New("platforms are FACEBOOK and INSTAGRAM")
		}
		if !seen[p] {
			seen[p] = true
			cleaned = append(cleaned, p)
		}
	}
	in.Platforms = cleaned
	switch {
	case in.Caption == "" && in.ImageURL == "":
		return pgtype.Timestamptz{}, errors.New("write a caption or add an image")
	case utf8.RuneCountInString(in.Caption) > maxCaptionLen:
		return pgtype.Timestamptz{}, errors.New("the caption is too long")
	case len(in.Platforms) == 0:
		return pgtype.Timestamptz{}, errors.New("pick at least one platform")
	case in.ImageURL != "" && !(strings.HasPrefix(in.ImageURL, "https://") || strings.HasPrefix(in.ImageURL, "http://")):
		return pgtype.Timestamptz{}, errors.New("the image must be a web address")
	case slices.Contains(in.Platforms, PlatformInstagram) && in.ImageURL == "":
		return pgtype.Timestamptz{}, errors.New("Instagram posts need an image")
	}
	if in.LinkID != "" {
		if _, err := uuid.Parse(in.LinkID); err != nil {
			return pgtype.Timestamptz{}, errors.New("unknown link")
		}
	}
	if s := strings.TrimSpace(in.ScheduledAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return pgtype.Timestamptz{}, errors.New("scheduled_at must be an RFC 3339 time")
		}
		return pgutil.Timestamptz(t), nil
	}
	return pgtype.Timestamptz{}, nil
}

func postView(p sqlc.SocialPost, link *sqlc.MarketingLink, slug string, urls URLs) map[string]any {
	var results map[string]any
	_ = json.Unmarshal(p.Results, &results)
	out := map[string]any{
		"id": pgutil.UUIDString(p.ID), "caption": p.Caption, "image_url": p.ImageUrl, "platforms": p.Platforms,
		"link_id": pgutil.UUIDPtr(p.LinkID), "status": p.Status, "scheduled_at": ts(p.ScheduledAt),
		"published_at": ts(p.PublishedAt), "results": results, "created_at": ts(p.CreatedAt), "updated_at": ts(p.UpdatedAt),
	}
	if link != nil {
		out["link_url"] = urls.Go(link.Code)
		// What to paste when sharing by hand.
		out["share_text"] = strings.TrimSpace(p.Caption + "\n" + urls.Go(link.Code))
	} else {
		out["share_text"] = p.Caption
	}
	_ = slug
	return out
}

/* ---------- handlers ---------- */

func (h *Handler) ListPosts(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	t, _ := tenantctx.FromContext(r.Context())
	rows, err := h.q.ListSocialPosts(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		h.internal(w, "load posts", err)
		return
	}
	links := map[pgtype.UUID]*sqlc.MarketingLink{}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		var link *sqlc.MarketingLink
		if p.LinkID.Valid {
			if l, ok := links[p.LinkID]; ok {
				link = l
			} else if l, err := h.q.GetLink(r.Context(), sqlc.GetLinkParams{ID: p.LinkID, TenantID: pgutil.UUID(tenantID)}); err == nil {
				links[p.LinkID] = &l
				link = &l
			}
		}
		out = append(out, postView(p, link, t.Slug, h.urls))
	}
	rd := readiness(r.Context(), h.cfg, tenantID)
	response.JSON(w, http.StatusOK, map[string]any{
		"posts": out,
		"connected": map[string]bool{
			PlatformFacebook: rd.Facebook == nil, PlatformInstagram: rd.Instagram == nil,
		},
	})
}

func (h *Handler) savePost(w http.ResponseWriter, r *http.Request, existing *sqlc.SocialPost) {
	tenantID, _, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var in PostInput
	if !decode(r, &in) {
		response.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	when, err := in.clean()
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	linkID, err := h.linkFor(r, tenantID, in.LinkID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// A time means scheduled; without a connection it will become MANUAL when due.
	status := SocialDraft
	if when.Valid {
		status = SocialScheduled
	}
	var p sqlc.SocialPost
	if existing == nil {
		p, err = h.q.CreateSocialPost(r.Context(), sqlc.CreateSocialPostParams{
			TenantID: pgutil.UUID(tenantID), Caption: in.Caption, ImageUrl: in.ImageURL, Platforms: in.Platforms,
			LinkID: linkID, Status: status, ScheduledAt: when,
		})
	} else {
		p, err = h.q.UpdateSocialPost(r.Context(), sqlc.UpdateSocialPostParams{
			Caption: in.Caption, ImageUrl: in.ImageURL, Platforms: in.Platforms, LinkID: linkID, Status: status,
			ScheduledAt: when, ID: existing.ID, TenantID: pgutil.UUID(tenantID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			response.Error(w, http.StatusConflict, "not_editable", "A post that is publishing or published cannot be changed.")
			return
		}
	}
	if err != nil {
		h.internal(w, "save post", err)
		return
	}
	var link *sqlc.MarketingLink
	if p.LinkID.Valid {
		if l, err := h.q.GetLink(r.Context(), sqlc.GetLinkParams{ID: p.LinkID, TenantID: pgutil.UUID(tenantID)}); err == nil {
			link = &l
		}
	}
	t, _ := tenantctx.FromContext(r.Context())
	code := http.StatusOK
	if existing == nil {
		code = http.StatusCreated
	}
	response.JSON(w, code, postView(p, link, t.Slug, h.urls))
}

func (h *Handler) CreatePost(w http.ResponseWriter, r *http.Request) { h.savePost(w, r, nil) }

func (h *Handler) UpdatePost(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	p, err := h.q.GetSocialPost(r.Context(), sqlc.GetSocialPostParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	h.savePost(w, r, &p)
}

func (h *Handler) DeletePost(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	n, err := h.q.DeleteSocialPost(r.Context(), sqlc.DeleteSocialPostParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		h.internal(w, "delete post", err)
		return
	}
	if n == 0 {
		response.Error(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublishPost schedules a post for now; the worker picks it up on its next
// tick so the API never blocks on Meta.
func (h *Handler) PublishPost(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := tenantOf(r)
	id, okID := idParam(r, "id")
	if !ok || !okID {
		response.Error(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	p, err := h.q.GetSocialPost(r.Context(), sqlc.GetSocialPostParams{ID: id, TenantID: pgutil.UUID(tenantID)})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	updated, err := h.q.PublishSocialPostNow(r.Context(), sqlc.PublishSocialPostNowParams{ID: p.ID, TenantID: pgutil.UUID(tenantID)})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusConflict, "not_editable", "This post is already publishing or published.")
		return
	}
	if err != nil {
		h.internal(w, "publish post", err)
		return
	}
	t, _ := tenantctx.FromContext(r.Context())
	response.JSON(w, http.StatusOK, postView(updated, nil, t.Slug, h.urls))
}

/* ---------- worker ---------- */

// publishSocial publishes due posts. One Graph API call per platform; the
// per-platform result is kept so a half-published post says which half.
func (w *Worker) publishSocial(ctx context.Context) {
	due, err := w.q.ClaimDueSocialPosts(ctx, socialBatch)
	if err != nil {
		w.log.Error("marketing social: claim", "error", err)
		return
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		w.publishOne(ctx, p)
	}
}

func (w *Worker) publishOne(ctx context.Context, p sqlc.SocialPost) {
	tenantID := uuid.UUID(p.TenantID.Bytes)
	results := map[string]any{}
	finish := func(status string) {
		raw, _ := json.Marshal(results)
		if _, err := w.q.FinishSocialPost(ctx, sqlc.FinishSocialPostParams{ID: p.ID, Status: status, Results: raw}); err != nil {
			w.log.Error("marketing social: finish", "error", err)
		}
	}
	provider, err := metaFor(ctx, w.cfg, tenantID)
	connected := err == nil && ((slices.Contains(p.Platforms, PlatformFacebook) && provider.CanFacebook()) ||
		(slices.Contains(p.Platforms, PlatformInstagram) && provider.CanInstagram()))
	if !connected {
		// Nothing to publish through: hand the post back to the owner to
		// share by hand, with the caption and link ready.
		results["reason"] = "No connected account for these platforms. Share this post yourself."
		finish(SocialManual)
		return
	}
	link := ""
	if p.LinkID.Valid {
		if l, err := w.q.GetLink(ctx, sqlc.GetLinkParams{ID: p.LinkID, TenantID: p.TenantID}); err == nil {
			link = w.urls.Go(l.Code)
		}
	}
	okCount, failCount := 0, 0
	for _, platform := range p.Platforms {
		callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		var id string
		var perr error
		switch platform {
		case PlatformFacebook:
			if !provider.CanFacebook() {
				perr = errors.New("no Facebook page connected")
			} else {
				id, perr = provider.PublishFacebook(callCtx, p.Caption, link, p.ImageUrl)
			}
		case PlatformInstagram:
			if !provider.CanInstagram() {
				perr = errors.New("no Instagram account connected")
			} else {
				caption := p.Caption
				if link != "" {
					caption = strings.TrimSpace(caption + "\n" + link)
				}
				id, perr = provider.PublishInstagram(callCtx, p.ImageUrl, caption)
			}
		}
		cancel()
		if perr != nil {
			failCount++
			results[platform] = map[string]any{"ok": false, "error": configsvc.SafeErrorMessage(perr)}
			continue
		}
		okCount++
		results[platform] = map[string]any{"ok": true, "id": id}
	}
	switch {
	case failCount == 0:
		finish(SocialPublished)
	case okCount == 0:
		finish(SocialFailed)
	default:
		finish(SocialPartial)
	}
}

var _ = chi.URLParam
