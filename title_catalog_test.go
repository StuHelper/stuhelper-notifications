package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"strings"
	"testing"
)

func TestTitleCatalogAdmissionAndRejectedWrites(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var contract struct {
		Catalog map[string]struct {
			Type           string   `json:"type"`
			Kinds          []string `json:"target_kinds"`
			Params         []string `json:"parameters"`
			OptionalParams []string `json:"optional_parameters"`
		} `json:"x-title-catalog"`
		Components struct {
			Schemas struct {
				ReasonCode struct {
					Enum []string `json:"enum"`
				} `json:"ReasonCode"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if e := json.Unmarshal(n.OpenAPI(), &contract); e != nil {
		t.Fatal(e)
	}
	if len(contract.Catalog) != 33 {
		t.Fatal("catalog coverage changed", len(contract.Catalog))
	}
	serial := 0
	for key, rule := range contract.Catalog {
		for _, kind := range rule.Kinds {
			serial++
			d := n.Event{Recipients: []string{"alice"}, ID: fmt.Sprint(serial), Subject: fmt.Sprint(serial), Revision: 1, EventType: "test.result.v1", Type: rule.Type, Source: "test", Target: n.Target{Kind: kind, ID: "object-1"}, TitleKey: key, TitleParams: map[string]string{}}
			for _, param := range rule.Params {
				d.TitleParams[param] = "ref-123"
			}
			v := deliver(t, f, d)
			if v.Recipients[0].ID == "" || v.Recipients[0].SuppressionReason != "" {
				t.Fatal(key, v)
			}
			if len(rule.OptionalParams) > 0 && rule.OptionalParams[0] == "reason_code" {
				serial++
				withReason := d
				withReason.ID = fmt.Sprint(serial)
				withReason.Subject = fmt.Sprint(serial)
				withReason.TitleParams = map[string]string{"reason_code": contract.Components.Schemas.ReasonCode.Enum[0]}
				for _, param := range rule.Params {
					withReason.TitleParams[param] = "ref-123"
				}
				v = deliver(t, f, withReason)
				if v.Recipients[0].ID == "" || v.Recipients[0].SuppressionReason != "" {
					t.Fatal(key, v)
				}
			}
		}
	}
	if count(t, f, "items") != serial || count(t, f, "inbox") != serial {
		t.Fatal("catalog deliveries missing")
	}
	mutations := map[string]func(*n.Event){
		"unknown key":                func(d *n.Event) { d.TitleKey = "notifications.unknown" },
		"missing parameters":         func(d *n.Event) { d.TitleParams = nil },
		"missing required parameter": func(d *n.Event) { d.TitleParams = map[string]string{"wrong": "ref-1"} },
		"extra parameter":            func(d *n.Event) { d.TitleParams["body"] = "private" },
		"caller count":               func(d *n.Event) { d.TitleParams["count"] = "99" },
		"wrong type":                 func(d *n.Event) { d.Type = "security" },
		"wrong target":               func(d *n.Event) { d.Target.Kind = "teacher_application" },
		"empty":                      func(d *n.Event) { d.TitleParams["content_id"] = "" },
		"long":                       func(d *n.Event) { d.TitleParams["content_id"] = strings.Repeat("a", 65) },
		"body text":                  func(d *n.Event) { d.TitleParams["content_id"] = "full content text" },
		"email":                      func(d *n.Event) { d.TitleParams["content_id"] = "person@example.invalid" },
		"control":                    func(d *n.Event) { d.TitleParams["content_id"] = "id\nsecret" },
		"invalid UTF8":               func(d *n.Event) { d.TitleParams["content_id"] = string([]byte{255}) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d := delivery("invalid-"+name, "interaction")
			mutate(&d)
			if _, e := f.s.Consume(ctx, d); !errors.Is(e, n.ErrInput) {
				t.Fatal(e)
			}
			if count(t, f, "items") != serial || count(t, f, "inbox") != serial {
				t.Fatal("rejected delivery wrote data")
			}
		})
	}
	// Preference suppression must not hide invalid producer input or record its key.
	disabled := false
	if _, e := f.s.Preferences(ctx, "alice", &disabled); e != nil {
		t.Fatal(e)
	}
	d := delivery("off-invalid", "interaction")
	d.TitleKey = "notifications.unknown"
	if _, e := f.s.Consume(ctx, d); !errors.Is(e, n.ErrInput) {
		t.Fatal(e)
	}
	if count(t, f, "inbox") != serial {
		t.Fatal("invalid suppressed inbox")
	}
	enabled := true
	if _, e := f.s.Preferences(ctx, "alice", &enabled); e != nil {
		t.Fatal(e)
	}
	d = delivery("max-reference", "interaction")
	d.TitleParams["content_id"] = strings.Repeat("a", 64)
	deliver(t, f, d)

	// Verify closed reason_code validation: arbitrary text, unknown enum values, and non-rejection keys are rejected.
	for _, badReason := range []string{"", "unknown_reason_code", "审核员手写备注", "duplicate_material ", "duplicate-material"} {
		rej := n.Event{
			Recipients:  []string{"alice"},
			ID:          "bad-reason-" + badReason,
			Subject:     "mod/bad-reason",
			Revision:    1,
			EventType:   "moderation.result.v1",
			Type:        "business",
			Source:      "moderation",
			Target:      n.Target{Kind: "review", ID: "review-1"},
			TitleKey:    "notifications.content.rejected",
			TitleParams: map[string]string{"content_id": "review-1", "reason_code": badReason},
		}
		if _, e := f.s.Consume(ctx, rej); !errors.Is(e, n.ErrInput) {
			t.Fatalf("expected ErrInput for reason_code %q, got %v", badReason, e)
		}
	}
	approvedWithReason := n.Event{
		Recipients:  []string{"alice"},
		ID:          "approved-with-reason",
		Subject:     "mod/approved-reason",
		Revision:    1,
		EventType:   "moderation.result.v1",
		Type:        "business",
		Source:      "moderation",
		Target:      n.Target{Kind: "review", ID: "review-1"},
		TitleKey:    "notifications.content.approved",
		TitleParams: map[string]string{"content_id": "review-1", "reason_code": "duplicate_material"},
	}
	if _, e := f.s.Consume(ctx, approvedWithReason); !errors.Is(e, n.ErrInput) {
		t.Fatalf("expected ErrInput for reason_code on approved title, got %v", e)
	}
}

func TestInteractionTitleUsesServerAggregateCount(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, key := range []string{"notifications.interaction.reply", "notifications.interaction.like"} {
		for i := range 2 {
			d := delivery(fmt.Sprintf("%s-%d", key, i), "interaction")
			d.TitleKey = key
			deliver(t, f, d)
		}
	}
	page, e := f.s.List(ctx, "alice", "", 100)
	if e != nil || len(page.Items) != 2 {
		t.Fatal(page, e)
	}
	for _, item := range page.Items {
		if item.Count != 2 || len(item.TitleParams) != 1 || item.TitleParams["content_id"] != "example-1" {
			t.Fatal(item)
		}
	}
}
