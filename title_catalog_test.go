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
			Type   string   `json:"type"`
			Kinds  []string `json:"target_kinds"`
			Params []string `json:"parameters"`
		} `json:"x-title-catalog"`
	}
	if e := json.Unmarshal(n.OpenAPI(), &contract); e != nil {
		t.Fatal(e)
	}
	if len(contract.Catalog) != 23 {
		t.Fatal("catalog coverage changed", len(contract.Catalog))
	}
	serial := 0
	for key, rule := range contract.Catalog {
		for _, kind := range rule.Kinds {
			serial++
			d := n.Delivery{Recipient: "alice", Key: fmt.Sprint(serial), Type: rule.Type, SourceModule: "test", Target: n.Target{Kind: kind, ID: "object-1"}, TitleKey: key, TitleParams: map[string]string{}}
			for _, param := range rule.Params {
				d.TitleParams[param] = "ref-123"
			}
			v := deliver(t, f, d)
			if v.ID == "" || v.Suppressed {
				t.Fatal(key, v)
			}
		}
	}
	if count(t, f, "items") != serial || count(t, f, "receipts") != serial {
		t.Fatal("catalog deliveries missing")
	}
	mutations := map[string]func(*n.Delivery){
		"unknown key":                func(d *n.Delivery) { d.TitleKey = "notifications.unknown" },
		"missing parameters":         func(d *n.Delivery) { d.TitleParams = nil },
		"missing required parameter": func(d *n.Delivery) { d.TitleParams = map[string]string{"wrong": "ref-1"} },
		"extra parameter":            func(d *n.Delivery) { d.TitleParams["body"] = "private" },
		"caller count":               func(d *n.Delivery) { d.TitleParams["count"] = "99" },
		"wrong type":                 func(d *n.Delivery) { d.Type = "security" },
		"wrong target":               func(d *n.Delivery) { d.Target.Kind = "teacher_application" },
		"empty":                      func(d *n.Delivery) { d.TitleParams["content_id"] = "" },
		"long":                       func(d *n.Delivery) { d.TitleParams["content_id"] = strings.Repeat("a", 65) },
		"body text":                  func(d *n.Delivery) { d.TitleParams["content_id"] = "full content text" },
		"email":                      func(d *n.Delivery) { d.TitleParams["content_id"] = "person@example.invalid" },
		"control":                    func(d *n.Delivery) { d.TitleParams["content_id"] = "id\nsecret" },
		"invalid UTF8":               func(d *n.Delivery) { d.TitleParams["content_id"] = string([]byte{255}) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d := delivery("invalid-"+name, "interaction")
			mutate(&d)
			if _, e := f.s.Deliver(ctx, d); !errors.Is(e, n.ErrInput) {
				t.Fatal(e)
			}
			if count(t, f, "items") != serial || count(t, f, "receipts") != serial {
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
	if _, e := f.s.Deliver(ctx, d); !errors.Is(e, n.ErrInput) {
		t.Fatal(e)
	}
	if count(t, f, "receipts") != serial {
		t.Fatal("invalid suppressed receipt")
	}
	enabled := true
	if _, e := f.s.Preferences(ctx, "alice", &enabled); e != nil {
		t.Fatal(e)
	}
	d = delivery("max-reference", "interaction")
	d.TitleParams["content_id"] = strings.Repeat("a", 64)
	deliver(t, f, d)
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
