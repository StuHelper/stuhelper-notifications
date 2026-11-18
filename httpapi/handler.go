package httpapi

import (
	"encoding/json"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Handler struct {
	Service      *n.Service
	Authenticate func(*http.Request) (string, error)
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.Service == nil || h.Authenticate == nil {
		failure(w, n.ErrUnavailable)
		return
	}
	owner, e := h.Authenticate(r)
	if e != nil {
		failure(w, e)
		return
	}
	if owner == "" {
		failure(w, n.ErrUnauthenticated)
		return
	}
	const prefix = "/api/v1/notifications"
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		failure(w, n.ErrNotFound)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	ctx := r.Context()
	readID, isSingleRead := singleSegmentReadID(path)

	var out any
	switch {
	case r.Method == http.MethodGet && path == "":
		query, qErr := url.ParseQuery(r.URL.RawQuery)
		if qErr != nil {
			failure(w, n.ErrInput)
			return
		}
		for key, values := range query {
			if len(values) != 1 || (key != "limit" && key != "cursor" && key != "include_content_names") {
				failure(w, n.ErrInput)
				return
			}
		}
		limit := 30
		if values, ok := query["limit"]; ok {
			limit, e = strconv.Atoi(values[0])
			if e != nil {
				failure(w, n.ErrInput)
				return
			}
		}
		includeNames := false
		if values, ok := query["include_content_names"]; ok {
			if values[0] != "true" && values[0] != "false" {
				failure(w, n.ErrInput)
				return
			}
			includeNames = values[0] == "true"
		}
		cursor := ""
		if values, ok := query["cursor"]; ok {
			cursor = values[0]
		}
		if includeNames {
			out, e = h.Service.ListWithContentNames(ctx, owner, cursor, limit)
		} else {
			out, e = h.Service.List(ctx, owner, cursor, limit)
		}
	case r.Method == http.MethodGet && path == "/unread-count":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		var count int64
		count, e = h.Service.Unread(ctx, owner)
		out = struct {
			Count int64 `json:"count"`
		}{count}
	case r.Method == http.MethodGet && path == "/preferences":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.Preferences(ctx, owner, nil)
	case r.Method == http.MethodPut && path == "/preferences":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		var in struct {
			Enabled *bool `json:"interaction_enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || in.Enabled == nil {
			failure(w, n.ErrInput)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.Preferences(ctx, owner, in.Enabled)
	case r.Method == http.MethodPost && path == "/read-all":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		e = h.Service.MarkAllRead(ctx, owner)
	case r.Method == http.MethodPost && isSingleRead:
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		e = h.Service.MarkRead(ctx, owner, readID)
	default:
		e = n.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	if out == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func singleSegmentReadID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/read")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func emptyQuery(raw string) bool {
	if raw == "" {
		return true
	}
	q, err := url.ParseQuery(raw)
	return err == nil && len(q) == 0
}

func failure(w http.ResponseWriter, e error) {
	code := "unavailable"
	status := http.StatusServiceUnavailable
	for _, entry := range []struct {
		err    error
		status int
	}{{n.ErrInput, 400}, {n.ErrUnauthenticated, 401}, {n.ErrForbidden, 403}, {n.ErrNotFound, 404}} {
		if errors.Is(e, entry.err) {
			status = entry.status
			code = entry.err.Error()
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{code})
}
