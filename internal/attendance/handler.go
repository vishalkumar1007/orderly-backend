// Package attendance is staff punch in / punch out: a plan feature
// (entitlement.FeatureAttendance) the shop also switches on for itself, with
// an optional location rule around the shop.
package attendance

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Handler struct {
	q *sqlc.Queries
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{q: sqlc.New(pool)}
}

func caller(r *http.Request) (identity.User, pgtype.UUID, bool) {
	u, ok := identity.UserFromContext(r.Context())
	if !ok || u.TenantID == nil {
		return identity.User{}, pgtype.UUID{}, false
	}
	return u, pgutil.UUID(*u.TenantID), true
}

// settings returns the shop's attendance settings, defaulting to switched off.
func (h *Handler) settings(r *http.Request, tenantID pgtype.UUID) (sqlc.TenantAttendanceSetting, error) {
	s, err := h.q.GetAttendanceSettings(r.Context(), tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.TenantAttendanceSetting{TenantID: tenantID, GeoRadiusM: 200}, nil
	}
	return s, err
}

func settingsJSON(s sqlc.TenantAttendanceSetting) map[string]any {
	out := map[string]any{
		"enabled":          s.Enabled,
		"require_location": s.RequireLocation,
		"geo_radius_m":     s.GeoRadiusM,
		"geo_lat":          nil,
		"geo_lng":          nil,
	}
	if s.GeoLat.Valid && s.GeoLng.Valid {
		out["geo_lat"], out["geo_lng"] = s.GeoLat.Float64, s.GeoLng.Float64
	}
	return out
}

func tsOrNil(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format(time.RFC3339)
}

func intOrNil(v pgtype.Int4) any {
	if !v.Valid {
		return nil
	}
	return v.Int32
}

func punchJSON(a sqlc.StaffAttendance) map[string]any {
	return map[string]any{
		"id":             pgutil.UUIDString(a.ID),
		"user_id":        pgutil.UUIDString(a.UserID),
		"clock_in_at":    tsOrNil(a.ClockInAt),
		"clock_out_at":   tsOrNil(a.ClockOutAt),
		"in_distance_m":  intOrNil(a.InDistanceM),
		"out_distance_m": intOrNil(a.OutDistanceM),
		"note":           a.Note,
		"edited":         a.EditedAt.Valid,
	}
}

// startOfDay is local midnight for the server's zone.
// ponytail: server time zone, not the shop's; store a tenant zone if shops span zones.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// Me is the punch widget's state: whether attendance is on, the open shift
// and time worked today.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	u, tid, ok := caller(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	s, err := h.settings(r, tid)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load attendance")
		return
	}
	out := settingsJSON(s)
	out["open"] = nil
	out["today_seconds"] = 0.0
	if !s.Enabled {
		response.JSON(w, http.StatusOK, out)
		return
	}
	uid := pgutil.UUID(u.ID)
	if open, err := h.q.GetOpenPunch(r.Context(), sqlc.GetOpenPunchParams{TenantID: tid, UserID: uid}); err == nil {
		out["open"] = punchJSON(open)
	}
	now := time.Now()
	secs, _ := h.q.WorkedSeconds(r.Context(), sqlc.WorkedSecondsParams{
		TenantID: tid, UserID: uid,
		FromAt: pgtype.Timestamptz{Time: startOfDay(now), Valid: true},
		ToAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	out["today_seconds"] = secs
	response.JSON(w, http.StatusOK, out)
}

type punchReq struct {
	Lat  *float64 `json:"lat"`
	Lng  *float64 `json:"lng"`
	Note string   `json:"note"`
}

// place validates the punch location against the shop's rule. It returns the
// coordinates and distance to store, or writes the refusal and returns false.
func place(w http.ResponseWriter, s sqlc.TenantAttendanceSetting, req punchReq) (lat, lng pgtype.Float8, dist pgtype.Int4, ok bool) {
	hasPos := req.Lat != nil && req.Lng != nil &&
		math.Abs(*req.Lat) <= 90 && math.Abs(*req.Lng) <= 180
	if !hasPos {
		if s.RequireLocation {
			response.Error(w, http.StatusBadRequest, "location_required", "Allow location access to punch in or out")
			return lat, lng, dist, false
		}
		return lat, lng, dist, true
	}
	lat = pgtype.Float8{Float64: *req.Lat, Valid: true}
	lng = pgtype.Float8{Float64: *req.Lng, Valid: true}
	if s.GeoLat.Valid && s.GeoLng.Valid {
		m := Distance(s.GeoLat.Float64, s.GeoLng.Float64, *req.Lat, *req.Lng)
		dist = pgtype.Int4{Int32: int32(math.Round(m)), Valid: true}
		if s.RequireLocation && m > float64(s.GeoRadiusM) {
			response.ErrorWithDetails(w, http.StatusForbidden, "outside_geofence",
				"You are too far from the shop to punch in or out",
				map[string]any{"distance_m": dist.Int32, "radius_m": s.GeoRadiusM})
			return lat, lng, dist, false
		}
	}
	return lat, lng, dist, true
}

func (h *Handler) punch(w http.ResponseWriter, r *http.Request, in bool) {
	u, tid, ok := caller(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req punchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > 500 {
		req.Note = req.Note[:500]
	}
	s, err := h.settings(r, tid)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load attendance")
		return
	}
	if !s.Enabled {
		response.Error(w, http.StatusConflict, "attendance_disabled", "Attendance is switched off for this shop")
		return
	}
	lat, lng, dist, ok := place(w, s, req)
	if !ok {
		return
	}
	uid := pgutil.UUID(u.ID)
	var row sqlc.StaffAttendance
	if in {
		row, err = h.q.PunchIn(r.Context(), sqlc.PunchInParams{
			TenantID: tid, UserID: uid, Lat: lat, Lng: lng, DistanceM: dist, Note: req.Note,
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			response.Error(w, http.StatusConflict, "already_punched_in", "You are already punched in")
			return
		}
	} else {
		row, err = h.q.PunchOut(r.Context(), sqlc.PunchOutParams{
			TenantID: tid, UserID: uid, Lat: lat, Lng: lng, DistanceM: dist, Note: req.Note,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			response.Error(w, http.StatusConflict, "not_punched_in", "You are not punched in")
			return
		}
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not record punch")
		return
	}
	response.JSON(w, http.StatusOK, punchJSON(row))
}

func (h *Handler) PunchIn(w http.ResponseWriter, r *http.Request)  { h.punch(w, r, true) }
func (h *Handler) PunchOut(w http.ResponseWriter, r *http.Request) { h.punch(w, r, false) }

// List is the timesheet: punches between from and to (RFC 3339 or
// YYYY-MM-DD), optionally for one person. Defaults to the last 7 days.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	_, tid, ok := caller(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	qs := r.URL.Query()
	to := parseWhen(qs.Get("to"), time.Now().Add(time.Minute))
	from := parseWhen(qs.Get("from"), startOfDay(to).AddDate(0, 0, -6))
	if !from.Before(to) || to.Sub(from) > 93*24*time.Hour {
		response.Error(w, http.StatusBadRequest, "invalid_range", "range must be positive and at most 93 days")
		return
	}
	params := sqlc.ListAttendanceParams{
		TenantID: tid,
		FromAt:   pgtype.Timestamptz{Time: from, Valid: true},
		ToAt:     pgtype.Timestamptz{Time: to, Valid: true},
	}
	if raw := qs.Get("user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user_id")
			return
		}
		params.UserID = pgutil.UUID(id)
	}
	rows, err := h.q.ListAttendance(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load attendance")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		p := punchJSON(sqlc.StaffAttendance{
			ID: row.ID, UserID: row.UserID, ClockInAt: row.ClockInAt, ClockOutAt: row.ClockOutAt,
			InDistanceM: row.InDistanceM, OutDistanceM: row.OutDistanceM, Note: row.Note, EditedAt: row.EditedAt,
		})
		p["user_name"] = row.UserName
		p["user_role"] = row.UserRole
		out = append(out, p)
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"punches": out,
		"from":    from.Format(time.RFC3339),
		"to":      to.Format(time.RFC3339),
	})
}

func parseWhen(raw string, def time.Time) time.Time {
	if raw == "" {
		return def
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, time.Local); err == nil {
		return t
	}
	return def
}

type editReq struct {
	ClockInAt  time.Time  `json:"clock_in_at"`
	ClockOutAt *time.Time `json:"clock_out_at"`
	Note       string     `json:"note"`
}

// Edit lets an owner or manager correct a punch. Every edit is audited with
// the before and after times.
func (h *Handler) Edit(w http.ResponseWriter, r *http.Request) {
	u, tid, ok := caller(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req editReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClockInAt.IsZero() {
		response.Error(w, http.StatusBadRequest, "invalid_request", "clock_in_at required")
		return
	}
	now := time.Now().Add(time.Minute)
	if req.ClockInAt.After(now) || (req.ClockOutAt != nil && (req.ClockOutAt.Before(req.ClockInAt) || req.ClockOutAt.After(now))) {
		response.Error(w, http.StatusBadRequest, "invalid_times", "punch out must follow punch in, and neither can be in the future")
		return
	}
	out := pgtype.Timestamptz{}
	if req.ClockOutAt != nil {
		out = pgtype.Timestamptz{Time: *req.ClockOutAt, Valid: true}
	}
	row, err := h.q.EditAttendance(r.Context(), sqlc.EditAttendanceParams{
		ID: pgutil.UUID(id), TenantID: tid,
		ClockInAt:  pgtype.Timestamptz{Time: req.ClockInAt, Valid: true},
		ClockOutAt: out,
		Note:       strings.TrimSpace(req.Note),
		EditedBy:   pgutil.UUID(u.ID),
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		response.Error(w, http.StatusNotFound, "not_found", "punch not found")
		return
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		response.Error(w, http.StatusConflict, "already_punched_in", "This person already has an open punch")
		return
	case err != nil:
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not update punch")
		return
	}
	meta, _ := json.Marshal(map[string]any{
		"staff_user_id": pgutil.UUIDString(row.UserID),
		"clock_in_at":   tsOrNil(row.ClockInAt),
		"clock_out_at":  tsOrNil(row.ClockOutAt),
	})
	_, _ = h.q.InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
		TenantID: tid, UserID: pgutil.UUID(u.ID), Action: "attendance.edited",
		EntityType: "attendance", EntityID: row.ID, Metadata: meta, Result: "SUCCESS",
	})
	response.JSON(w, http.StatusOK, punchJSON(row))
}

func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	_, tid, ok := caller(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	s, err := h.settings(r, tid)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load attendance settings")
		return
	}
	response.JSON(w, http.StatusOK, settingsJSON(s))
}

type settingsReq struct {
	Enabled         bool     `json:"enabled"`
	RequireLocation bool     `json:"require_location"`
	GeoLat          *float64 `json:"geo_lat"`
	GeoLng          *float64 `json:"geo_lng"`
	GeoRadiusM      int32    `json:"geo_radius_m"`
}

func (h *Handler) PutSettings(w http.ResponseWriter, r *http.Request) {
	u, tid, ok := caller(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req settingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	if req.GeoRadiusM == 0 {
		req.GeoRadiusM = 200
	}
	if req.GeoRadiusM < 20 || req.GeoRadiusM > 50000 {
		response.Error(w, http.StatusBadRequest, "invalid_radius", "radius must be between 20 and 50000 metres")
		return
	}
	params := sqlc.UpsertAttendanceSettingsParams{
		TenantID: tid, Enabled: req.Enabled, RequireLocation: req.RequireLocation, GeoRadiusM: req.GeoRadiusM,
	}
	if (req.GeoLat == nil) != (req.GeoLng == nil) {
		response.Error(w, http.StatusBadRequest, "invalid_location", "set both latitude and longitude, or neither")
		return
	}
	if req.GeoLat != nil {
		if math.Abs(*req.GeoLat) > 90 || math.Abs(*req.GeoLng) > 180 {
			response.Error(w, http.StatusBadRequest, "invalid_location", "invalid coordinates")
			return
		}
		params.GeoLat = pgtype.Float8{Float64: *req.GeoLat, Valid: true}
		params.GeoLng = pgtype.Float8{Float64: *req.GeoLng, Valid: true}
	}
	if req.RequireLocation && !params.GeoLat.Valid {
		response.Error(w, http.StatusBadRequest, "invalid_location", "set the shop location to require it")
		return
	}
	s, err := h.q.UpsertAttendanceSettings(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save attendance settings")
		return
	}
	_, _ = h.q.InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
		TenantID: tid, UserID: pgutil.UUID(u.ID), Action: "attendance.settings_updated",
		EntityType: "tenant", EntityID: tid, Metadata: []byte("{}"), Result: "SUCCESS",
	})
	response.JSON(w, http.StatusOK, settingsJSON(s))
}

// Distance is the great-circle distance in metres (haversine).
func Distance(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}
