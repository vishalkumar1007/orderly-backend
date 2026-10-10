package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// renderedMessage is what a channel actually sends: EMAIL has both a
// subject and a body, SMS only a body (Subject is ignored for SMS).
type renderedMessage struct {
	Subject string
	Body    string
}

// render produces the EMAIL or SMS copy for an event, preferring a tenant's
// own active template, falling back to the platform default template, and
// finally falling back to the caller-supplied title/body when no template
// row exists yet — so every event is sendable the moment it is wired, before
// anyone has visited the Templates screen.
//
// Substitution is plain string replacement of "{{var}}" tokens, never a
// template language: there is no control flow to abuse, so this is free of
// the injection surface a real template engine would need to be defended
// against. A token not in the event's declared variable allowlist is left
// untouched rather than guessed at.
func render(ctx context.Context, q *sqlc.Queries, tenantID *uuid.UUID, eventCode, channel, fallbackTitle, fallbackBody string, data map[string]any) renderedMessage {
	tmpl, err := q.GetEffectiveTemplate(ctx, sqlc.GetEffectiveTemplateParams{
		EventCode: eventCode,
		Channel:   channel,
		TenantID:  pgutil.NullUUID(tenantID),
	})
	if err != nil {
		if err != pgx.ErrNoRows {
			// Decryption/DB trouble: still fall back, never block the send.
			_ = err
		}
		return renderedMessage{Subject: fallbackTitle, Body: fallbackBody}
	}

	allowed := allowedVariables(ctx, q, eventCode)
	return renderedMessage{
		Subject: substitute(orFallback(tmpl.Subject, fallbackTitle), allowed, data),
		Body:    substitute(orFallback(tmpl.Body, fallbackBody), allowed, data),
	}
}

func orFallback(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// allowedVariables reads the event's declared variable names. A lookup
// failure degrades to "nothing substitutes" rather than erroring, since a
// catalog row should always exist for a code this package itself defined.
func allowedVariables(ctx context.Context, q *sqlc.Queries, eventCode string) []string {
	ev, err := q.GetNotificationEvent(ctx, eventCode)
	if err != nil {
		return nil
	}
	var out []string
	_ = json.Unmarshal(ev.Variables, &out)
	return out
}

// substitute replaces every "{{name}}" token whose name is in allowed with
// data[name], formatted with fmt.Sprint. Unknown tokens are left verbatim.
func substitute(body string, allowed []string, data map[string]any) string {
	if len(allowed) == 0 || len(data) == 0 {
		return body
	}
	pairs := make([]string, 0, len(allowed)*2)
	for _, name := range allowed {
		v, ok := data[name]
		if !ok {
			continue
		}
		pairs = append(pairs, "{{"+name+"}}", fmt.Sprint(v))
	}
	if len(pairs) == 0 {
		return body
	}
	return strings.NewReplacer(pairs...).Replace(body)
}
