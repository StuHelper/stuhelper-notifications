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
	w.Header().Set("Content-Type", "application/json")
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
	var out any
	switch {
	case r.Method == http.MethodGet && path == "":
		query := r.URL.Query()
		for k := range query {
			switch k {
			case "cursor", "limit", "include_content_names", "box", "type", "unread_only":
			default:
				failure(w, n.ErrInput)
				return
			}
		}
		limit := 30
		if values, ok := query["limit"]; ok {
			if len(values) != 1 {
				failure(w, n.ErrInput)
				return
			}
			limit, e = strconv.Atoi(values[0])
			if e != nil {
				failure(w, n.ErrInput)
				return
			}
		}
		cursor := ""
		if values, ok := query["cursor"]; ok {
			if len(values) != 1 || (values[0] != "" && strings.TrimSpace(values[0]) == "") {
				failure(w, n.ErrInput)
				return
			}
			cursor = values[0]
		}
		box, ok := singleQuery(query, "box")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		kind, ok := singleQuery(query, "type")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		includeNames, ok := boolQuery(query, "include_content_names")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		unreadOnly, ok := boolQuery(query, "unread_only")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.ListFiltered(ctx, owner, n.ListQuery{
			Cursor:              cursor,
			Limit:               limit,
			Box:                 box,
			Type:                kind,
			UnreadOnly:          unreadOnly,
			IncludeContentNames: includeNames,
		})
	case r.Method == http.MethodGet && path == "/unread-count":
		out, e = h.Service.UnreadSummary(ctx, owner)
	case r.Method == http.MethodGet && path == "/preferences":
		out, e = h.Service.Preferences(ctx, owner, nil)
	case r.Method == http.MethodPut && path == "/preferences":
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
		query := r.URL.Query()
		for k := range query {
			if k != "box" && k != "type" {
				failure(w, n.ErrInput)
				return
			}
		}
		box, ok := singleQuery(query, "box")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		kind, ok := singleQuery(query, "type")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		e = h.Service.MarkAllReadFiltered(ctx, owner, n.ReadFilter{Box: box, Type: kind})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/read"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/read")
		e = h.Service.MarkRead(ctx, owner, id)
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
	_ = json.NewEncoder(w).Encode(out)
}
func failure(w http.ResponseWriter, e error) {
	code := "unavailable"
	status := http.StatusServiceUnavailable
	for _, entry := range []struct {
		err    error
		status int
	}{{n.ErrInput, 400}, {n.ErrUnauthenticated, 401}, {n.ErrNotFound, 404}} {
		if errors.Is(e, entry.err) {
			status = entry.status
			code = entry.err.Error()
			break
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{code})
}

func singleQuery(q url.Values, name string) (string, bool) {
	values, ok := q[name]
	if !ok {
		return "", true
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", false
	}
	return values[0], true
}

func boolQuery(q url.Values, name string) (bool, bool) {
	values, ok := q[name]
	if !ok {
		return false, true
	}
	if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
		return false, false
	}
	return values[0] == "true", true
}
