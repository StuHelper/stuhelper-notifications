package notifications

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (s *Service) reportResolve(err error) {
	if s.config.Diagnostics == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.config.Diagnostics("target_resolve", ReasonResolveTimeout)
	case errors.Is(err, context.Canceled):
		s.config.Diagnostics("target_resolve", ReasonResolveCanceled)
	default:
		s.config.Diagnostics("target_resolve", ReasonResolveError)
	}
}

func (s *Service) resolvePage(ctx context.Context, owner string, items []Notification, includeNames bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
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
		if pErr := ctx.Err(); pErr != nil {
			return pErr
		}
		if err == nil {
			results = values
		} else {
			s.reportResolve(err)
		}
	} else {
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			call, cancel := context.WithTimeout(ctx, s.config.ResolveTimeout)
			state, err := s.resolver.Resolve(call, owner, ref.Source, ref.Target)
			cancel()
			if pErr := ctx.Err(); pErr != nil {
				return pErr
			}
			if err == nil {
				results[ref] = TargetResolution{Status: state}
			} else {
				s.reportResolve(err)
			}
		}
	}
	var accessible, inaccessible, unavailable int
	for _, ref := range refs {
		switch results[ref].Status {
		case "accessible":
			accessible++
		case "inaccessible":
			inaccessible++
		default:
			unavailable++
		}
	}
	for i := range items {
		item := &items[i]
		ref := TargetRef{item.SourceModule, item.Target}
		resolved, hasResolved := results[ref]
		state := resolved.Status
		if state != "accessible" && state != "inaccessible" {
			if hasResolved && state != "" && state != "unavailable" && s.config.Diagnostics != nil {
				s.config.Diagnostics("target_resolve", ReasonResolveInvalidStatus)
			}
			state = "unavailable"
		}
		item.TargetStatus = state
		// Clone to avoid retaining a name in a repository implementation's object.
		item.TitleParams = maps.Clone(item.TitleParams)
		delete(item.TitleParams, "content_name")
		delete(item.TitleParams, "content_name_state")
		delete(item.TitleParams, "context_id")
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
				if s.config.Diagnostics != nil {
					s.config.Diagnostics("target_resolve", ReasonResolveInvalidName)
				}
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
		if item.Target.Kind == "comment" && state == "accessible" && slices.Contains(rule.ReadParameters, "context_id") {
			if contextID := strings.TrimSpace(resolved.ContextID); validContextID(contextID) {
				item.TitleParams["context_id"] = contextID
			}
		}
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
	return nil
}

func validContextID(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && s != "." && s != ".." && !strings.ContainsFunc(s, unicode.IsControl) && !strings.ContainsAny(s, " /?#\\")
}
