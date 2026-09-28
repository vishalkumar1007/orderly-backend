package platform

import (
	"context"
	"net/http"
	"time"

	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/response"
)

/*
System health.

The console needs one answer to "what is failing?", and it has to be an honest
one. Every component below reports what this build can actually observe:

  - API and database are probed live on the request.
  - Email, storage and AI report the status the configuration stack recorded
    the last time each was saved or tested. Nothing is dialled here: a health
    check that opened an SMTP connection on every poll would be a slow health
    check and a good way to get rate-limited.
  - Payments and background processing have no platform-level provider in this
    build. They are listed as UNAVAILABLE with the reason, rather than omitted,
    because "we do not run that here" is the answer the operator needs.
*/

// Health statuses. A closed set, mirrored by the console's badges.
const (
	healthHealthy     = "HEALTHY"
	healthWarning     = "WARNING"
	healthError       = "ERROR"
	healthUnavailable = "UNAVAILABLE"
)

// startedAt is the process start, used for the API component's uptime.
var startedAt = time.Now()

type healthComponent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Group  string `json:"group"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// LatencyMS is set only for components that were probed on this request.
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	// Managed says whether a Super Admin can act on this component from the
	// console. The UI uses it to decide whether to offer a link.
	Managed bool   `json:"managed"`
	Href    string `json:"href,omitempty"`
}

// SystemHealth reports every component the platform depends on.
func (h *Handler) SystemHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	components := []healthComponent{
		h.apiComponent(),
		h.databaseComponent(ctx),
	}
	components = append(components, h.providerComponents(ctx)...)
	components = append(components,
		healthComponent{
			ID:     "payments",
			Name:   "Payments",
			Group:  "Providers",
			Status: healthUnavailable,
			Detail: "No platform payment gateway. Each business chooses cash or online at pickup in its own storefront settings.",
		},
		healthComponent{
			ID:     "background",
			Name:   "Background processing",
			Group:  "Platform",
			Status: healthUnavailable,
			Detail: "No worker or queue is deployed. Invite email and provider tests run inline on the request that triggers them.",
		},
	)

	worst := healthHealthy
	for _, c := range components {
		switch c.Status {
		case healthError:
			worst = healthError
		case healthWarning:
			if worst != healthError {
				worst = healthWarning
			}
		}
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"status":     worst,
		"checked_at": time.Now().UTC().Format(time.RFC3339),
		"components": components,
	})
}

func (h *Handler) apiComponent() healthComponent {
	up := time.Since(startedAt).Round(time.Second)
	return healthComponent{
		ID:     "api",
		Name:   "API",
		Group:  "Platform",
		Status: healthHealthy,
		Detail: "Serving this request. Up " + humanDuration(up) + ".",
	}
}

func (h *Handler) databaseComponent(ctx context.Context) healthComponent {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	err := h.pool.Ping(probeCtx)
	latency := time.Since(start).Milliseconds()

	c := healthComponent{
		ID:        "database",
		Name:      "Database",
		Group:     "Platform",
		LatencyMS: &latency,
	}
	switch {
	case err != nil:
		c.Status = healthError
		// The driver's message can name a host and port; that is operational
		// detail a Super Admin needs, and it carries no credential.
		c.Detail = "PostgreSQL is not reachable: " + truncateDetail(err.Error())
	case latency > 500:
		c.Status = healthWarning
		c.Detail = "PostgreSQL answered slowly."
	default:
		c.Status = healthHealthy
		stat := h.pool.Stat()
		c.Detail = "PostgreSQL responding. " +
			itoa(int(stat.AcquiredConns())) + " of " + itoa(int(stat.MaxConns())) + " connections in use."
	}
	return c
}

// providerComponents maps each configured integration onto a health row. The
// configuration stack is the source of truth; this never reads a secret.
func (h *Handler) providerComponents(ctx context.Context) []healthComponent {
	meta := map[configsvc.ServiceType]struct{ name, detailNoun string }{
		configsvc.ServiceSMTP:    {"Email", "Outbound email"},
		configsvc.ServiceStorage: {"Storage", "Object storage"},
		configsvc.ServiceAI:      {"AI", "The AI provider"},
	}

	out := make([]healthComponent, 0, len(configsvc.AllServices))
	if !h.configsEnabled() {
		for _, service := range configsvc.AllServices {
			out = append(out, healthComponent{
				ID:     lower(string(service)),
				Name:   meta[service].name,
				Group:  "Providers",
				Status: healthUnavailable,
				Detail: "Configuration is disabled on this deployment.",
			})
		}
		return out
	}

	records, err := h.configs.Store().ListPlatform(ctx)
	if err != nil {
		for _, service := range configsvc.AllServices {
			out = append(out, healthComponent{
				ID:     lower(string(service)),
				Name:   meta[service].name,
				Group:  "Providers",
				Status: healthWarning,
				Detail: "Could not read the stored configuration.",
			})
		}
		return out
	}

	for _, service := range configsvc.AllServices {
		info := meta[service]
		c := healthComponent{
			ID:      lower(string(service)),
			Name:    info.name,
			Group:   "Providers",
			Managed: true,
			Href:    "/superadmin/providers/" + lower(string(service)),
		}
		record := records[service]
		switch {
		case record == nil || record.Status == configsvc.StatusUnconfigured:
			c.Status = healthUnavailable
			c.Detail = info.detailNoun + " is not configured at platform level."
		case record.Status == configsvc.StatusConnectionFailed:
			c.Status = healthError
			c.Detail = "Last connection test failed."
			if record.LastError != "" {
				c.Detail = "Last connection test failed: " + truncateDetail(record.LastError)
			}
		case !record.Enabled || record.Status == configsvc.StatusDisabled:
			c.Status = healthWarning
			c.Detail = "Configured but switched off."
		case record.Status == configsvc.StatusConfigured:
			// Saved and on, but never verified. That is worth flagging: the
			// first time anyone finds out is otherwise a failed invite email.
			c.Status = healthWarning
			c.Detail = "Enabled but never tested. Run a connection test."
		default:
			c.Status = healthHealthy
			c.Detail = providerHealthyDetail(record)
		}
		out = append(out, c)
	}
	return out
}

func providerHealthyDetail(record *configsvc.Record) string {
	detail := "Enabled"
	if record.Provider != "" {
		detail += " · " + record.Provider
	}
	if record.LastTestedAt != nil {
		detail += " · verified " + record.LastTestedAt.Format("2 Jan 15:04")
	}
	if record.AllowTenants {
		detail += " · shared with businesses"
	}
	return detail + "."
}

/* ---------- small helpers, kept local to the health surface ---------- */

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h"
	default:
		return itoa(int(d.Hours()/24)) + "d"
	}
}

func truncateDetail(s string) string {
	const max = 180
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
