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
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			failure(w, n.ErrInput)
			return
		}
		for k := range query {
			switch k {
			case "cursor", "limit", "box", "type", "unread_only":
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
		unreadOnly, ok := boolQuery(query, "unread_only")
		if !ok {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.ListFiltered(ctx, owner, n.ListQuery{
			Cursor:     cursor,
			Limit:      limit,
			Box:        box,
			Type:       kind,
			UnreadOnly: unreadOnly,
		})
	case r.Method == http.MethodGet && path == "/unread-count":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.UnreadSummary(ctx, owner)
	case r.Method == http.MethodGet && path == "/preferences":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.Preferences(ctx, owner)
	case r.Method == http.MethodPut && path == "/preferences":
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		var in struct {
			Reply strictBool `json:"reply_enabled"`
			Like  strictBool `json:"like_enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || (!in.Reply.set && !in.Like.set) {
			failure(w, n.ErrInput)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.UpdatePreferences(ctx, owner, n.PreferencesUpdate{
			ReplyEnabled: in.Reply.ptr(),
			LikeEnabled:  in.Like.ptr(),
		})
	case r.Method == http.MethodPost && path == "/read-all":
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			failure(w, n.ErrInput)
			return
		}
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
	case r.Method == http.MethodPost && isSingleRead:
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		e = h.Service.MarkRead(ctx, owner, readID)
	case r.Method == http.MethodDelete && singleItemPath(path):
		if !emptyQuery(r.URL.RawQuery) {
			failure(w, n.ErrInput)
			return
		}
		e = h.Service.Delete(ctx, owner, strings.TrimPrefix(path, "/"))

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

type strictBool struct {
	set   bool
	value bool
}

func (b *strictBool) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return n.ErrInput
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	b.set = true
	b.value = v
	return nil
}

func (b strictBool) ptr() *bool {
	if !b.set {
		return nil
	}
	v := b.value
	return &v
}

func singleItemPath(path string) bool {
	id, ok := strings.CutPrefix(path, "/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return false
	}
	switch id {
	case "preferences", "unread-count", "read-all":
		return false
	default:
		return true
	}
}
