package notifications

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (s *Service) resolvePage(ctx context.Context, owner string, items []Notification, includeNames bool) {
	if len(items) == 0 {
		return
	}
	started := time.Now()
	refs := []TargetRef{}
	seen := map[TargetRef]bool{}
	for _, item := range items {
		ref := TargetRef{item.SourceModule, item.Target}
		if !seen[ref] {
			refs = append(refs, ref)
			seen[ref] = true
		}
	}
	results := map[TargetRef]TargetResolution{}
	batch, isBatch := s.resolver.(BatchResolver)
	if isBatch {
		call, cancel := context.WithTimeout(ctx, s.config.ResolveTimeout)
		values, err := batch.ResolveBatch(call, owner, refs)
		cancel()
		if err == nil {
			results = values
		}
	} else {
		for _, ref := range refs {
			call, cancel := context.WithTimeout(ctx, s.config.ResolveTimeout)
			state, err := s.resolver.Resolve(call, owner, ref.Source, ref.Target)
			cancel()
			if err == nil {
				results[ref] = TargetResolution{Status: state}
			}
		}
	}
	var accessible, inaccessible, unavailable int
	for i := range items {
		item := &items[i]
		resolved := results[TargetRef{item.SourceModule, item.Target}]
		state := resolved.Status
		switch state {
		case "accessible":
			accessible++
		case "inaccessible":
			inaccessible++
		default:
			state = "unavailable"
			unavailable++
		}
		item.TargetStatus = state
		// Clone to avoid retaining a name in a repository implementation's object.
		item.TitleParams = maps.Clone(item.TitleParams)
		delete(item.TitleParams, "content_name")
		delete(item.TitleParams, "content_name_state")
		rule := titleCatalog[item.TitleKey]
		if !includeNames || !slices.Contains(rule.ReadParameters, "content_name") || !slices.Contains([]string{"review", "comment", "resource"}, item.Target.Kind) {
			continue
		}
		name := strings.TrimSpace(resolved.Name)
		nameState := state
		if state == "accessible" {
			if name != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 256 && !strings.ContainsFunc(name, unicode.IsControl) {
				nameState = "available"
			} else {
				nameState = "unavailable"
			}
		}
		if nameState != "available" {
			name = ""
		}
		if item.TitleParams == nil {
			item.TitleParams = map[string]string{}
		}
		item.TitleParams["content_name"] = name
		item.TitleParams["content_name_state"] = nameState
	}
	if s.config.Observer != nil {
		s.config.Observer.ObserveResolve(ResolveObservation{
			Batch:        isBatch,
			Targets:      len(refs),
			Accessible:   accessible,
			Inaccessible: inaccessible,
			Unavailable:  unavailable,
			Duration:     time.Since(started),
		})
	}
}
