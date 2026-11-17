package httpapi

import (
	"encoding/json"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"io"
	"net/http"
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
		limit := 30
		if r.URL.Query().Has("limit") {
			limit, e = strconv.Atoi(r.URL.Query().Get("limit"))
			if e != nil {
				failure(w, n.ErrInput)
				return
			}
		}
		includeNames := false
		if values, ok := r.URL.Query()["include_content_names"]; ok {
			if len(values) != 1 || values[0] != "true" && values[0] != "false" {
				failure(w, n.ErrInput)
				return
			}
			includeNames = values[0] == "true"
		}
		if includeNames {
			out, e = h.Service.ListWithContentNames(ctx, owner, r.URL.Query().Get("cursor"), limit)
		} else {
			out, e = h.Service.List(ctx, owner, r.URL.Query().Get("cursor"), limit)
		}
	case r.Method == http.MethodGet && path == "/unread-count":
		var count int64
		count, e = h.Service.Unread(ctx, owner)
		out = struct {
			Count int64 `json:"count"`
		}{count}
	case r.Method == http.MethodGet && path == "/preferences":
		out, e = h.Service.Preferences(ctx, owner, nil)
	case r.Method == http.MethodPut && path == "/preferences":
		var in struct {
			Interaction strictBool `json:"interaction_enabled"`
			Reply       strictBool `json:"reply_enabled"`
			Like        strictBool `json:"like_enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || (!in.Interaction.set && !in.Reply.set && !in.Like.set) {
			failure(w, n.ErrInput)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			failure(w, n.ErrInput)
			return
		}
		out, e = h.Service.UpdatePreferences(ctx, owner, n.PreferencesUpdate{
			InteractionEnabled: in.Interaction.ptr(),
			ReplyEnabled:       in.Reply.ptr(),
			LikeEnabled:        in.Like.ptr(),
		})
	case r.Method == http.MethodPost && path == "/read-all":
		e = h.Service.MarkAllRead(ctx, owner)
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
