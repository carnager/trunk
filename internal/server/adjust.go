package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/carnager/trunk/internal/places"
	"golang.org/x/text/language"
)

// Correcting when and where photos were taken: for scans, cameras with a
// wrong clock, files that lost their metadata on the way. Only the database
// changes; the files stay as they came, so their own EXIF data is still there
// to go back to (and reindex would bring it back).

// PlaceHit is a town found by name.
type PlaceHit struct {
	ID      int64   `json:"id"`
	City    string  `json:"city"`
	Region  string  `json:"region,omitempty"`
	Country string  `json:"country,omitempty"`
	CC      string  `json:"cc"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// handlePlaces: GET /api/v1/places?q=wien[&limit=10] → {"places": [...]}, in
// the caller's language: exact names first, then bigger towns.
func (s *Server) handlePlaces(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	tags, _, _ := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	out := []PlaceHit{}
	for _, p := range places.Search(r.URL.Query().Get("q"), limit) {
		n := p.In(tags...)
		out = append(out, PlaceHit{p.ID, n.City, n.Region, n.Country, n.CC, p.Lat, p.Lon})
	}
	writeJSON(w, http.StatusOK, map[string]any{"places": out})
}

// handleAdjust: POST /api/v1/assets/adjust with "ids" and any of:
//   - "shift_minutes": move every photo's time by this much (a camera clock
//     that was off);
//   - "local_time" ("2023-05-01T14:00:00"), optional "tz_offset" (minutes
//     east of UTC): the earliest photo is set to this time and the others
//     keep their distance to it, so a series stays in order;
//   - "place_id" (from GET /places): taken in this town; a position the
//     photos had is dropped, being wrong;
//   - "clear_place": true: no place, no position.
//
// → {"assets": [...]} as they are now.
func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs          []int64 `json:"ids"`
		ShiftMinutes *int64  `json:"shift_minutes"`
		LocalTime    *string `json:"local_time"`
		TZOffset     *int    `json:"tz_offset"`
		PlaceID      *int64  `json:"place_id"`
		ClearPlace   bool    `json:"clear_place"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 5000 {
		httpError(w, http.StatusBadRequest, "need 1 to 5000 ids")
		return
	}
	ctx := r.Context()
	me := sessionFrom(r).UserID
	var list []*Asset
	for _, id := range req.IDs {
		a, err := s.getAsset(ctx, me, id)
		if err != nil {
			httpError(w, http.StatusNotFound, "not your photo: "+strconv.FormatInt(id, 10))
			return
		}
		list = append(list, a)
	}
	if req.PlaceID != nil && places.ByID(*req.PlaceID) == nil {
		httpError(w, http.StatusBadRequest, "unknown place")
		return
	}

	var delta time.Duration
	switch {
	case req.ShiftMinutes != nil:
		delta = time.Duration(*req.ShiftMinutes) * time.Minute
	case req.LocalTime != nil:
		local, err := time.Parse("2006-01-02T15:04:05", *req.LocalTime)
		if err != nil {
			httpError(w, http.StatusBadRequest, "local_time: want 2006-01-02T15:04:05")
			return
		}
		first := list[0]
		for _, a := range list {
			if a.TakenAt.Before(*first.TakenAt) {
				first = a
			}
		}
		off := 0
		if req.TZOffset != nil {
			off = *req.TZOffset
		} else if first.TZOffset != nil {
			off = *first.TZOffset
		}
		target := local.Add(-time.Duration(off) * time.Minute) // the instant, in UTC
		delta = target.Sub(*first.TakenAt)
	}
	if req.TZOffset != nil && (*req.TZOffset < -14*60 || *req.TZOffset > 14*60) {
		httpError(w, http.StatusBadRequest, "tz_offset out of range")
		return
	}

	ids := make([]int64, len(list))
	for i, a := range list {
		ids[i] = a.ID
	}
	var err error
	if delta != 0 {
		err = s.mutate(ctx, me, ids, `taken_at = taken_at + ?`, delta.Milliseconds())
	}
	if err == nil && req.TZOffset != nil {
		err = s.mutate(ctx, me, ids, `tz_offset = ?`, *req.TZOffset)
	}
	switch {
	case err != nil:
	case req.PlaceID != nil:
		err = s.mutate(ctx, me, ids, `place_id = ?, lat = NULL, lon = NULL, alt = NULL`, *req.PlaceID)
	case req.ClearPlace:
		// 0, not NULL: "no place" must not be filled in again from a position.
		err = s.mutate(ctx, me, ids, `place_id = 0, lat = NULL, lon = NULL, alt = NULL`)
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]*Asset, 0, len(ids))
	for _, id := range ids {
		if a, err := s.getAsset(ctx, me, id); err == nil {
			localize(withLanguage(ctx, r), a)
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": out})
}
