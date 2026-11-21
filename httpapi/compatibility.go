package httpapi

import (
	"maps"
	"net/url"

	n "github.com/StuHelper/stuhelper-notifications"
)

func legacyNotificationsPage(page n.Page) n.Page {
	for i := range page.Items {
		item := &page.Items[i]
		_, hasContext := item.TitleParams["context_id"]
		_, hasReason := item.TitleParams["reason_code"]
		if hasContext || hasReason {
			item.TitleParams = maps.Clone(item.TitleParams)
			delete(item.TitleParams, "context_id")
			delete(item.TitleParams, "reason_code")
		}
	}
	return page
}

func preferencesView(prefs n.Preferences, extended bool) any {
	if extended {
		return prefs
	}
	return struct {
		InteractionEnabled bool `json:"interaction_enabled"`
	}{InteractionEnabled: prefs.InteractionEnabled}
}

func extendedOnlyQuery(raw string) (extended bool, valid bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return false, false
	}
	for name := range query {
		if name != "include_extended" {
			return false, false
		}
	}
	return boolQuery(query, "include_extended")
}
